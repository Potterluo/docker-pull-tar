package config

import (
	"bufio"
	"os"
	"strings"
)

// loadDotEnv reads KEY=VALUE pairs from .env (current working
// directory) into the process environment. Real environment variables
// ALWAYS win — .env only fills gaps — so container manifests and shell
// exports can override whatever a developer left in the file.
//
// Supported: blank lines, # comments, KEY=VALUE, optional
// single/double quotes around the value, inline # comments after a
// space. No variable expansion — keep it boring.
//
// Called once from Load; missing file is not an error.
func loadDotEnv() {
	file, err := os.Open(".env")
	if err != nil {
		return
	}
	defer file.Close()

	sc := bufio.NewScanner(file)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 &&
			((value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		if key == "" {
			continue
		}
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, value)
		}
	}
}
