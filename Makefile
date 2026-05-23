.PHONY: docs-generate test-apiserver

docs-generate:
	go run ./hack/docgen

test-apiserver:
	tests/apiserver/run.sh
