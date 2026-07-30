#!/bin/sh
set -eu

/Library/PrivilegedHelperTools/submux-runtime-lifecycle \
  post-install '@VERSION@' '@KIND@'
exit 0
