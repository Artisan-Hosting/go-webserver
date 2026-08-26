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
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		lineNum := i + 1
		line := strings.TrimSpace(lines[i])
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

		if quote := unclosedQuote(value); quote != 0 {
			// A quoted value left open runs until a line ends with the
			// matching quote, so list-shaped settings (STATUS_SERVICES) can be
			// written one entry per line instead of on one unreadable line.
			value, i = readQuotedValue(lines, i, value, quote)
			if !strings.HasSuffix(strings.TrimRight(lines[i], " \t\r"), string(quote)) {
				log.Printf("%s:%d: %s has an unterminated %c-quoted value; reading to end of file", path, lineNum, key, quote)
			}
		} else {
			value = strings.Trim(value, `"'`)
		}

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

// unclosedQuote reports the quote character a value opens with but does not
// close on the same line, or 0 when the value is complete as written.
func unclosedQuote(value string) byte {
	if value == "" {
		return 0
	}
	quote := value[0]
	if quote != '"' && quote != '\'' {
		return 0
	}
	if len(value) >= 2 && value[len(value)-1] == quote {
		return 0
	}
	return quote
}

// readQuotedValue consumes lines from start until one ends with quote,
// returning the assembled value and the index of the last line consumed.
// Interior newlines are preserved; an unterminated value runs to end of file.
func readQuotedValue(lines []string, start int, first string, quote byte) (string, int) {
	var value strings.Builder
	value.WriteString(strings.TrimPrefix(first, string(quote)))

	for i := start + 1; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t\r")
		value.WriteString("\n")
		if strings.HasSuffix(line, string(quote)) {
			value.WriteString(strings.TrimSuffix(line, string(quote)))
			return value.String(), i
		}
		value.WriteString(lines[i])
	}
	return value.String(), len(lines) - 1
}
