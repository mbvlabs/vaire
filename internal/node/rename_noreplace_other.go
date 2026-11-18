//go:build !linux && !darwin

package node

import "errors"

// renameNoReplaceAt fails closed where the platform has no native atomic
// no-replace rename; a check-then-rename fallback would be racy.
func renameNoReplaceAt(int, string, int, string) error {
	return errors.New("atomic no-replace rename is unsupported on this platform")
}
