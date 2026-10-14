#!/bin/sh
set -e
printf '%s\n' "${GREETING}" > /usr/local/share/itest-feature-greeting
printf '%s\n' "${_REMOTE_USER:-}" > /usr/local/share/itest-feature-remote-user
printf '#!/bin/sh\necho itest-feature-ok\n' > /usr/local/bin/itest-feature
chmod 0755 /usr/local/bin/itest-feature
