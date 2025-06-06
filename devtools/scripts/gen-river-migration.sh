#!/bin/sh

set -e

VERSION=$1

if [ -z "$VERSION" ]; then
    echo "Usage: $0 <migration-version>"
    exit 1
fi

go tool river migrate-get --version $VERSION --up >/dev/null 2>&1 || {
    echo "Migration version $VERSION does not exist."
    exit 1
}

TARGET=$(make new-migration NAME=import-river-migration-$VERSION | awk '{print $2}')

echo "-- Code generated by ./devtools/scripts/gen-river-migration.sh. DO NOT EDIT." >$TARGET

echo "" >>$TARGET
echo "-- +migrate Up" >>$TARGET

go tool river migrate-get --version $VERSION --up >>$TARGET

echo "" >>$TARGET
echo "-- +migrate Down" >>$TARGET

go tool river migrate-get --version $VERSION --down >>$TARGET
