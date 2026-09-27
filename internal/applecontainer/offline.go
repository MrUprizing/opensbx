package applecontainer

import (
	"errors"
	"strings"
)

var errOfflineImageUnavailable = errors.New("Apple container cannot create offline: the workload or configured vminit image is not fully available locally; explicitly prepare the missing local images before retrying; automatic downloads remain disabled")

// Apple 1.4.1 Utility.containerConfigFromFlags fetches both the workload and
// vminit with the same download limit. ClientImage.fetch first resolves locally;
// its fallback pull rejects zero before sending any registry request. A missing
// vminit is therefore an environment prerequisite failure, not a port collision.
// Recognize only the exact version-pinned diagnostic under our own zero-download
// flag. Never expose the original stderr, which may contain caller secrets.
func offlineImageUnavailable(args []string, diagnostic string, truncated bool) bool {
	return !truncated && len(args) >= 3 &&
		args[0] == "create" && args[1] == "--max-concurrent-downloads" && args[2] == "0" &&
		strings.TrimSpace(diagnostic) == "Error: maximum number of concurrent downloads must be greater than 0, got 0"
}
