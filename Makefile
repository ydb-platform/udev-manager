IMAGE := golang:1.24-bookworm
SETUP  := apt-get update -qq && apt-get install -y -q libudev-dev

LINT_IMAGE := golangci/golangci-lint:v2.1.6

.PHONY: test build coverage lint kind-up kind-down kind-device e2e

test:
	docker run --rm -v "$(CURDIR):/go/app" -w /go/app $(IMAGE) \
		sh -c "$(SETUP) && CGO_ENABLED=1 go test -race -count=1 -timeout 60s ./..."

build:
	docker run --rm -v "$(CURDIR):/go/app" -w /go/app $(IMAGE) \
		sh -c "$(SETUP) && CGO_ENABLED=1 go build ./..."

coverage:
	docker run --rm -v "$(CURDIR):/go/app" -w /go/app $(IMAGE) \
		sh -c "$(SETUP) && CGO_ENABLED=1 go test -race -count=1 -timeout 60s -coverprofile=coverage.out ./... && go tool cover -func=coverage.out"

lint:
	docker run --rm -v "$(CURDIR):/go/app" -w /go/app $(LINT_IMAGE) \
		sh -c "$(SETUP) && golangci-lint run ./..."

kind-up:
	bash test/kind/kind.sh up

kind-down:
	bash test/kind/kind.sh down

kind-device:
	bash test/kind/kind.sh device "$(ACTION)" "$(NODE)" "$(LABEL)" "$(SIGNATURE)" "$(DEVICE)" "$(PEER)" "$(RDMA_DEVICE)"

e2e:
	bash test/kind/e2e.sh
