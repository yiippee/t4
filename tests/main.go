package main

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/t4db/t4"
	"github.com/t4db/t4/pkg/object"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	cfg := object.S3Config{
		Bucket:          "openim",
		AccessKeyID:     "root",
		SecretAccessKey: "openIM123",
		Prefix:          "t4/restore-demo/",
		Region:          "us-east-1",
		Endpoint:        "http://47.238.6.72:10005",
	}

	if err := ensureBucket(ctx, cfg); err != nil {
		panic(err)
	}

	store, err := object.NewS3StoreFromConfig(ctx, cfg)
	if err != nil {
		panic(err)
	}

	dataDir := "./t4-data"

	node, err := t4.Open(t4.Config{
		DataDir:     dataDir,
		ObjectStore: store,
	})
	if err != nil {
		panic(err)
	}
	defer node.Close()

	key := "/config/timeout-1"
	kv, err := node.Get(key)
	if err != nil {
		panic(err)
	}
	if kv == nil {
		fmt.Println("key not found")
	} else {
		fmt.Println("get value:", string(kv.Value))
	}

	rev, err := node.Put(ctx, key, []byte("30s"), 0)
	if err != nil {
		panic(err)
	}
	fmt.Println("put revision:", rev)

	if _, err := node.Put(ctx, "/config/retries-1", []byte("3"), 0); err != nil {
		panic(err)
	}

	kv, err = node.Get(key)
	if err != nil {
		panic(err)
	}
	fmt.Println("get value:", string(kv.Value))

	kvs, err := node.List("/config/")
	if err != nil {
		panic(err)
	}
	fmt.Println("list /config/:")
	for _, item := range kvs {
		fmt.Printf("  %s=%s\n", item.Key, item.Value)
	}

	events, err := node.Watch(ctx, "/config/", 0)
	if err != nil {
		panic(err)
	}

	if _, err := node.Put(ctx, key, []byte("45s"), 0); err != nil {
		panic(err)
	}

	select {
	case e := <-events:
		fmt.Printf("watch event: %s %s=%s\n", eventType(e.Type), e.KV.Key, e.KV.Value)
	case <-ctx.Done():
		panic(ctx.Err())
	}
}

func ensureBucket(ctx context.Context, cfg object.S3Config) error {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return fmt.Errorf("parse endpoint: %w", err)
	}
	if u.Host == "" {
		return fmt.Errorf("endpoint %q has empty host", cfg.Endpoint)
	}

	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, cfg.SessionToken),
		Secure: u.Scheme == "https",
		Region: cfg.Region,
	})
	if err != nil {
		return fmt.Errorf("create minio client: %w", err)
	}

	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return fmt.Errorf("check bucket %q: %w", cfg.Bucket, err)
	}
	if exists {
		return nil
	}
	if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{Region: cfg.Region}); err != nil {
		return fmt.Errorf("create bucket %q: %w", cfg.Bucket, err)
	}
	return nil
}

func eventType(t t4.EventType) string {
	switch t {
	case t4.EventPut:
		return "put"
	case t4.EventDelete:
		return "delete"
	default:
		return fmt.Sprintf("unknown(%d)", t)
	}
}
