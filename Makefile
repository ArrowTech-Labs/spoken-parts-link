VERSION ?= $(shell sed -n 's/^version: *"\{0,1\}\([^"]*\)"\{0,1\} *$$/\1/p' spoken_parts_link/config.yaml)
LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)

.PHONY: test build image fmt vet

test:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/linux-amd64/spoken-parts-link ./cmd/spoken-parts-link
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -buildvcs=false -ldflags "$(LDFLAGS)" -o dist/linux-arm64/spoken-parts-link ./cmd/spoken-parts-link

image:
	docker buildx build -f spoken_parts_link/Dockerfile --build-arg VERSION=$(VERSION) -t spoken-parts-link:$(VERSION) --load .
