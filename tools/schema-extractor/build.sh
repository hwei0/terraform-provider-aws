#!/bin/bash
set -e

echo "Building schema-extractor..."

# Save the original go.mod and go.sum
cp ../../go.mod ../../go.mod.backup
cp ../../go.sum ../../go.sum.backup

# Temporarily change the Go version requirement
sed -i.tmp 's/^go 1\.24\.11$/go 1.24.9/' ../../go.mod

# Update dependencies with the new Go version
echo "Updating dependencies..."
cd ../..
go mod tidy
go mod download github.com/hashicorp/terraform-plugin-sdk/v2
cd tools/schema-extractor

# Build the tool with -mod=mod to allow go.sum updates
echo "Building..."
go build -mod=mod -o schema-extractor

# Restore the original go.mod and go.sum
mv ../../go.mod.backup ../../go.mod
mv ../../go.sum.backup ../../go.sum
rm -f ../../go.mod.tmp

echo "Build complete! Run with: ./schema-extractor output.json"