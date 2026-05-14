package e2e_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type minioConfig struct {
	bin      string
	endpoint string
	bucket   string
	prefix   string
	access   string
	secret   string
	region   string
}

func TestMinIOCLISmoke(t *testing.T) {
	if os.Getenv("T4_E2E_MINIO") == "" {
		t.Skip("set T4_E2E_MINIO=1 to run the MinIO-backed CLI smoke test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	workDir := t.TempDir()
	cfg := minioConfig{
		bin:      envOr("T4_SMOKE_BIN", ""),
		endpoint: envOr("MINIO_ENDPOINT", "http://127.0.0.1:9000"),
		bucket:   envOr("MINIO_BUCKET", fmt.Sprintf("t4-smoke-%d", time.Now().UnixNano())),
		prefix:   envOr("T4_SMOKE_PREFIX", fmt.Sprintf("smoke-%d", time.Now().UnixNano())),
		access:   envOr("MINIO_ACCESS_KEY", "minioadmin"),
		secret:   envOr("MINIO_SECRET_KEY", "minioadmin"),
		region:   envOr("MINIO_REGION", "us-east-1"),
	}
	if cfg.bin == "" {
		cfg.bin = buildT4(t, ctx, workDir)
	}

	if err := ensureBucket(ctx, cfg); err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}

	listenAddr := freeAddr(t)
	dataDir := filepath.Join(workDir, "node")
	node, nodeLog := startNode(t, ctx, cfg, dataDir, listenAddr)
	defer stopNode(node)

	if err := writeData(ctx, listenAddr); err != nil {
		t.Fatalf("write data: %v\nnode log:\n%s", err, nodeLog.String())
	}
	restoreDir, err := waitForRestoredCount(ctx, cfg, workDir, "/smoke/", 3)
	if err != nil {
		t.Fatalf("wait for restorable data: %v\nnode log:\n%s", err, nodeLog.String())
	}

	statusArgs := append([]string{"status"}, s3Args(cfg)...)
	if out, err := runT4(ctx, cfg, statusArgs...); err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	} else if !strings.Contains(out, "Latest checkpoint") {
		t.Fatalf("status output missing checkpoint section:\n%s", out)
	}

	if out, err := runT4(ctx, cfg, "inspect", "count", "--data-dir", restoreDir, "--prefix", "/smoke/"); err != nil {
		t.Fatalf("inspect restored data: %v\n%s", err, out)
	} else if lastNonEmptyLine(out) != "/smoke/: 3" {
		t.Fatalf("unexpected restored count %q", strings.TrimSpace(out))
	}

	branchArgs := append([]string{"branch", "fork", "--branch-id", "smoke-branch"}, s3Args(cfg)...)
	branchOut, err := runT4(ctx, cfg, branchArgs...)
	if err != nil {
		t.Fatalf("branch fork: %v\n%s", err, branchOut)
	}
	branchKey := lastNonEmptyLine(branchOut)
	if !strings.HasPrefix(branchKey, "checkpoint/") {
		t.Fatalf("unexpected branch checkpoint key %q from output:\n%s", branchKey, branchOut)
	}

	gcArgs := append([]string{"gc", "--keep", "1"}, s3Args(cfg)...)
	if out, err := runT4(ctx, cfg, gcArgs...); err != nil {
		t.Fatalf("gc with pinned branch: %v\n%s", err, out)
	} else if !strings.Contains(out, "GC complete") {
		t.Fatalf("gc output missing success text:\n%s", out)
	}

	unforkArgs := append([]string{"branch", "unfork", "--branch-id", "smoke-branch"}, s3Args(cfg)...)
	if out, err := runT4(ctx, cfg, unforkArgs...); err != nil {
		t.Fatalf("branch unfork: %v\n%s", err, out)
	}

	if out, err := runT4(ctx, cfg, gcArgs...); err != nil {
		t.Fatalf("gc after unfork: %v\n%s", err, out)
	}
}

func buildT4(t *testing.T, ctx context.Context, workDir string) string {
	t.Helper()
	binPath := filepath.Join(workDir, "t4")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", binPath, "./cmd/t4")
	cmd.Dir = filepath.Join("..", "..")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("build t4: %v\n%s", err, out.String())
	}
	return binPath
}

func ensureBucket(ctx context.Context, cfg minioConfig) error {
	client, err := s3Client(ctx, cfg)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(cfg.bucket)})
		if err == nil {
			return nil
		}
		if _, headErr := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(cfg.bucket)}); headErr == nil {
			return nil
		}
		lastErr = err
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("create bucket %q: %w", cfg.bucket, lastErr)
}

func s3Client(ctx context.Context, cfg minioConfig) (*s3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(cfg.access, cfg.secret, "")),
		awsconfig.WithRegion(cfg.region),
		awsconfig.WithBaseEndpoint(cfg.endpoint),
	)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) { o.UsePathStyle = true }), nil
}

func startNode(t *testing.T, ctx context.Context, cfg minioConfig, dataDir, listenAddr string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	args := []string{
		"run",
		"--data-dir", dataDir,
		"--listen", listenAddr,
		"--metrics-addr", "127.0.0.1:0",
		"--checkpoint-entries", "1",
		"--checkpoint-interval-min", "1",
		"--segment-max-age-sec", "1",
		"--log-level", "warn",
	}
	args = append(args, s3Args(cfg)...)

	var log bytes.Buffer
	cmd := exec.CommandContext(ctx, cfg.bin, args...)
	cmd.Stdout = &log
	cmd.Stderr = &log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start t4 node: %v", err)
	}
	if err := waitForEtcd(ctx, listenAddr); err != nil {
		stopNode(cmd)
		t.Fatalf("wait for etcd endpoint: %v\nnode log:\n%s", err, log.String())
	}
	return cmd, &log
}

func writeData(ctx context.Context, listenAddr string) error {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{listenAddr},
		DialTimeout: 2 * time.Second,
	})
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	for i := 0; i < 3; i++ {
		_, err := cli.Put(ctx, fmt.Sprintf("/smoke/%d", i), fmt.Sprintf("value-%d", i))
		if err != nil {
			return err
		}
	}
	return nil
}

func waitForEtcd(ctx context.Context, listenAddr string) error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := writeProbe(ctx, listenAddr); err == nil {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("timed out waiting for t4 etcd endpoint")
}

func writeProbe(ctx context.Context, listenAddr string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{listenAddr},
		DialTimeout: time.Second,
	})
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	_, err = cli.Put(probeCtx, "/__smoke_probe", "ready")
	return err
}

func waitForRestoredCount(ctx context.Context, cfg minioConfig, workDir, prefix string, want int) (string, error) {
	deadline := time.Now().Add(45 * time.Second)
	wantOut := fmt.Sprintf("%s: %d", prefix, want)
	var lastOut string
	for time.Now().Before(deadline) {
		restoreDir := filepath.Join(workDir, fmt.Sprintf("restored-%d", time.Now().UnixNano()))
		restoreArgs := append([]string{"restore", "checkpoint", "--data-dir", restoreDir}, s3Args(cfg)...)
		restoreOut, err := runT4(ctx, cfg, restoreArgs...)
		if err == nil && strings.Contains(restoreOut, "Restored checkpoint") {
			countOut, countErr := runT4(ctx, cfg, "inspect", "count", "--data-dir", restoreDir, "--prefix", prefix)
			if countErr == nil && lastNonEmptyLine(countOut) == wantOut {
				return restoreDir, nil
			}
			lastOut = countOut
			if countErr != nil {
				lastOut = fmt.Sprintf("%v\n%s", countErr, countOut)
			}
		} else {
			lastOut = restoreOut
			if err != nil {
				lastOut = fmt.Sprintf("%v\n%s", err, restoreOut)
			}
		}
		_ = os.RemoveAll(restoreDir)
		time.Sleep(500 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for restored %q count %d; last output:\n%s", prefix, want, lastOut)
}

func runT4(ctx context.Context, cfg minioConfig, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, cfg.bin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func s3Args(cfg minioConfig) []string {
	return []string{
		"--s3-bucket", cfg.bucket,
		"--s3-prefix", cfg.prefix,
		"--s3-endpoint", cfg.endpoint,
		"--s3-region", cfg.region,
		"--s3-access-key-id", cfg.access,
		"--s3-secret-access-key", cfg.secret,
	}
}

func stopNode(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lis.Close() }()
	return lis.Addr().String()
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func lastNonEmptyLine(out string) string {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			return line
		}
	}
	return ""
}
