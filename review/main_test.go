package review

import (
	"os"
	"testing"
)

// Parallel tests cannot use t.Setenv. The git env is the same for each test.
func TestMain(m *testing.M) {
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":   os.DevNull,
		"GIT_CONFIG_NOSYSTEM": "1",
		"GIT_AUTHOR_NAME":     "t",
		"GIT_AUTHOR_EMAIL":    "t@t",
		"GIT_COMMITTER_NAME":  "t",
		"GIT_COMMITTER_EMAIL": "t@t",
	} {
		if err := os.Setenv(k, v); err != nil {
			panic(err)
		}
	}
	os.Exit(m.Run())
}
