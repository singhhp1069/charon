#!/usr/bin/env bash

if ! which buf 1>/dev/null; then
  echo "Installing tools"
  go generate tools.go
fi

buf generate
buf lint

# Disabling since this always results in "files were modified by this hook"
# buf breaking --against '.git#branch=origin/main'
