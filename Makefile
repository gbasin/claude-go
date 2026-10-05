.PHONY: test test-e2e test-all build vet

build:
	go build ./...

vet:
	go vet ./... && go vet -tags e2e ./e2e/

test:
	go test ./...

# PTY + recording-server end-to-end suite (no credentials needed — the
# mock serves API traffic; aux tests run against an in-process recorder)
test-e2e:
	go test -tags e2e -count=1 ./e2e/

test-all: vet test test-e2e
