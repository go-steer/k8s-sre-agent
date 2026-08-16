package evals

import (
	"bytes"
	"os"
)

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

func containsKey(blob []byte, key string) bool {
	return bytes.Contains(blob, []byte(`"`+key+`"`))
}
