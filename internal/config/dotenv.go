package config

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ReadEnv parses a deliberately small dotenv grammar. No shell expansion,
// command substitution, or multiline values are executed or interpreted.
func ReadEnv(r io.Reader) (map[string]string, error) {
	data, err := io.ReadAll(io.LimitReader(r, 1024*1024+1))
	if err != nil {
		return nil, fmt.Errorf("read dotenv: %w", err)
	}
	if len(data) > 1024*1024 {
		return nil, fmt.Errorf("dotenv exceeds 1 MiB")
	}
	values := make(map[string]string)
	keyPattern := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 4096), 1024*1024)
	line := 0
	for s.Scan() {
		line++
		text := strings.TrimSpace(s.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || !keyPattern.MatchString(key) {
			return nil, fmt.Errorf("dotenv line %d: expected KEY=value", line)
		}
		if _, exists := values[key]; exists {
			return nil, fmt.Errorf("dotenv line %d: duplicate key %s", line, key)
		}
		if strings.HasPrefix(value, `"`) {
			decoded, err := strconv.Unquote(value)
			if err != nil {
				return nil, fmt.Errorf("dotenv line %d: invalid quoted value", line)
			}
			value = decoded
		} else if strings.HasPrefix(value, "'") {
			if len(value) < 2 || !strings.HasSuffix(value, "'") {
				return nil, fmt.Errorf("dotenv line %d: unterminated quote", line)
			}
			value = value[1 : len(value)-1]
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		values[key] = value
	}
	if err := s.Err(); err != nil {
		return nil, fmt.Errorf("read dotenv: %w", err)
	}
	return values, nil
}

func loadEnv(path string, explicit bool) (map[string]string, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) && !explicit {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open dotenv: %w", err)
	}
	values, readErr := ReadEnv(f)
	closeErr := f.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close dotenv: %w", closeErr)
	}
	return values, nil
}
