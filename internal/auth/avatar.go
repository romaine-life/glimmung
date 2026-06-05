package auth

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"strings"
)

func GravatarURL(email string, size int) string {
	if size <= 0 {
		size = 64
	}
	normalized := strings.ToLower(strings.TrimSpace(email))
	sum := md5.Sum([]byte(normalized))
	return fmt.Sprintf("https://www.gravatar.com/avatar/%s?s=%d&d=mp", hex.EncodeToString(sum[:]), size)
}
