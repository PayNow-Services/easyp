package gitmodules

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	"github.com/easyp-tech/easyp/internal/adapters/gitcommand"
)

func gitV1(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := gitcommand.Run(ctx, dir, nil, args...)
	if err != nil {
		return "", fmt.Errorf("Run: %w", err)
	}
	return string(out), nil
}

// repositoryNotFound reports a remote that does not exist, as opposed to a
// server that refuses to serve a commit requested by its ID.
func repositoryNotFound(err error) bool {
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "repository not found") ||
		strings.Contains(message, "is not a valid repository name") ||
		(strings.Contains(message, "fatal: repository '") && strings.Contains(message, "' not found"))
}

func v1CacheSourceKey(source string) string {
	hash := sha256.Sum256([]byte(source))
	return fmt.Sprintf("%x", hash)
}
