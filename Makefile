IMAGE := golang:1.24-bookworm
SETUP  := apt-get update -qq && apt-get install -y -q libudev-dev

.PHONY: test build

test:
	docker run --rm -v "$(CURDIR):/go/app" -w /go/app $(IMAGE) \
		sh -c "$(SETUP) && CGO_ENABLED=1 go test -race -count=1 -timeout 60s ./..."

build:
	docker run --rm -v "$(CURDIR):/go/app" -w /go/app $(IMAGE) \
		sh -c "$(SETUP) && CGO_ENABLED=1 go build ./..."
