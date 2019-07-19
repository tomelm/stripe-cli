SOURCE_FILES?=./...
TEST_PATTERN?=.
TEST_OPTIONS?=

export GO111MODULE := on

# Install all the build and lint dependencies
setup:
	go mod download
.PHONY: setup

# Run all the tests
test:
	go test $(TEST_OPTIONS) -failfast -race -coverpkg=./... -covermode=atomic -coverprofile=coverage.txt $(SOURCE_FILES) -run $(TEST_PATTERN) -timeout=2m
.PHONY: test

# Run all the tests and opens the coverage report
cover: test
	go tool cover -html=coverage.txt
.PHONY: cover

# gofmt and goimports all go files
fmt:
	find . -name '*.go' -not -wholename './vendor/*' | while read -r file; do gofmt -w -s "$$file"; goimports -w "$$file"; done
.PHONY: fmt

# Run the linter
lint:
	golint -set_exit_status ./...
.PHONY: lint

# Run Vet
vet:
	go vet ./...
.PHONY: vet

# Clean go.mod
go-mod-tidy:
	@go mod tidy -v
	@git diff HEAD
	@git diff-index --quiet HEAD
.PHONY: go-mod-tidy

# Run all the tests and code checks
ci: build test lint vet go-mod-tidy
.PHONY: ci

# Build a beta version of stripe
build:
	go build -o stripe cmd/stripe/main.go
.PHONY: build

# Release a new version of stripe by creating a tag and pushing it to remote.
# The actual release is done by our CI upon detecting the new tag.
release:
	git pull origin master

# Makefile's execute each line in its own subshell so variables don't
# persist. Instead, grab the version and run the `tag` command in the same
# subprocess by escaping the newline
	@read -p "Enter new version (of the format vN.N.N): " version; \
	git tag $$version
	git push --tags
.PHONY: release

.DEFAULT_GOAL := build
