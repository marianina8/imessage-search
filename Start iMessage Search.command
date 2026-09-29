#!/bin/bash
# Double-click this file in Finder to start iMessage Search.
# The first run sets things up automatically, then your browser opens.
exec /bin/bash "$(dirname "$0")/scripts/launch.sh" imsg.server --open
