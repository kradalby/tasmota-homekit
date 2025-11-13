#!/usr/bin/env bash
# Hook script for golangci-lint pre-commit
# Runs golangci-lint on the entire project (not just changed files)

set -e

# Run golangci-lint on all Go files
golangci-lint run ./...
