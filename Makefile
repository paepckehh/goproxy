PROJECT=$(shell basename $(CURDIR))
VERSION ?= $(shell git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0-dev)
LDFLAGS := -X paepcke.de/goproxy/internal/version.Version=$(VERSION)

all: info

info: 
	echo "$(PROJECT) $(VERSION)"

build:
	touch $(PROJECT) && rm $(PROJECT)
	go build -ldflags "$(LDFLAGS)" -o ./${PROJECT} ./cmd/$(PROJECT)

update: 
	git pull
	git pull --tags 

push: update
	git push 
	git push --tags

deploy-test-nix: update build 
	sudo -v
	sudo mkdir -p /nix/persist/bin || true
	sudo touch /nix/persist/root/bin/$(PROJECT) || true
	sudo touch /nix/persist/root/bin/$(PROJECT).old || true
	sudo cp -af /nix/persist/root/bin/$(PROJECT).old /nix/persist/root/bin/$(PROJECT).old2 || true 
	sudo cp -af /nix/persist/root/bin/$(PROJECT) /nix/persist/root/bin/$(PROJECT).old || true 
	sudo systemctl stop go-mod-proxy01.service || true
	sudo systemctl stop go-mod-proxy02.service || true
	sudo cp -af ./$(PROJECT) /nix/persist/root/bin/$(PROJECT)
	sudo systemctl start go-mod-proxy02.service || true
	sudo systemctl start go-mod-proxy01.service || true

deps:
	rm -rf go.mod go.sum
	go mod init paepcke.de/$(PROJECT)
	go mod tidy -v
	git config core.fileMode false
check:
	@files="$$(gofmt -l .)"; if [ -n "$$files" ]; then echo "$$files"; exit 1; fi
	go vet ./...
	go mod tidy -diff

generate:
	go generate ./...

test:
	CGO_ENABLED=1 go test -race -count=1 -parallel $$(nproc) -p $$(nproc) ./...

test-e2e:
	GOPROXY_E2E=1 CGO_ENABLED=1 go test -race -count=1 -run E2E ./internal/proxy ./cmd/goproxy

screenshots:
	GOPROXY_SCREENSHOTS=1 go test ./internal/ui -run TestScreenshots -count=1 -v -timeout 5m

cloc:
	go run github.com/boyter/scc/v4@latest

release-check:
	CGO_ENABLED=0 goreleaser check

release-snapshot:
	CGO_ENABLED=0 goreleaser release --snapshot --clean
