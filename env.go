package main

import (
	"log"
	"os"
	"strings"
)

// loadDotEnv reads simple KEY=VALUE pairs from path into the process
// environment. It is a no-op if the file does not exist, and never
// overrides a variable that is already set (so real environment/systemd
// config always wins over the .env file).
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("failed to read env file %s: %v", path, err)
		}
		return
	}

	var loaded []string
	for i, line := range strings.Split(string(data), "\n") {
		lineNum := i + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			log.Printf("%s:%d: skipping line with no '=' separator", path, lineNum)
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			log.Printf("%s:%d: skipping line with empty key", path, lineNum)
			continue
		}
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)

		if _, exists := os.LookupEnv(key); exists {
			log.Printf("%s:%d: %s already set in the environment, keeping the existing value", path, lineNum, key)
			continue
		}
		os.Setenv(key, value)
		loaded = append(loaded, key)
	}

	if len(loaded) > 0 {
		log.Printf("loaded env vars from %s: %s", path, strings.Join(loaded, ", "))
	}
}
