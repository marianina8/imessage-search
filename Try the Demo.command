#!/bin/bash
# Double-click to try iMessage Search with fictional messages.
# Your own messages are not read or changed.
exec /bin/bash "$(dirname "$0")/scripts/launch.sh" imsg.demo --open
