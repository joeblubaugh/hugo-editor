#! /bin/bash

set -euxo pipefail

# Set up our git credentials
git config --global url."https://api:$GIT_TOKEN@github.com/".insteadOf "https://github.com/"
git config --global url."https://ssh:$GIT_TOKEN@github.com/".insteadOf "ssh://git@github.com/"
git config --global url."https://git:$GIT_TOKEN@github.com/".insteadOf "git@github.com:"

# Make sure our hugo directory is up-to-date with main. During installation,
# the user must clone this directory. This script doesn't set that up.
cd $SITE_PATH
git switch main
git pull

# Run the program until it exits
/usr/local/bin/hugo-editor/hugo-editor --site $SITE_DIR --server_port $SERVER_PORT --hugo-port $HUGO_SERVER_PORT --publish-cmd $PUBLISH_CMD

