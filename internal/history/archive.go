package history

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"ttc/internal/llm"
	"ttc/internal/privatefile"
)

// ArchiveMessages freezes an isolated actor's model input as Markdown and exact
// JSONL envelopes, including replay state and attachment bytes. The caller owns
// the supplied Markdown projection; managed files are immutable and private.
func (s *Store) ArchiveMessages(session, actor, markdown string, messages []llm.Message) (string, error) {
	var exact bytes.Buffer
	encoder := json.NewEncoder(&exact)
	for _, message := range messages {
		if err := encoder.Encode(struct {
			Actor   string      `json:"actor"`
			Message llm.Message `json:"message"`
		}{actor, message}); err != nil {
			return "", err
		}
	}
	return s.writeArchive(session, []byte(markdown), exact.Bytes())
}

func (s *Store) writeArchive(session string, text, exact []byte) (string, error) {
	v, err := s.Session(session)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	hash.Write(text)
	hash.Write(exact)
	dir := filepath.Join(s.Root, "lineages", v.LineageID, "compactions")
	if err := privatefile.PrivateDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%x.md", hash.Sum(nil)))
	for _, file := range []struct {
		path string
		data []byte
	}{{path, text}, {path + ".jsonl", exact}} {
		if actual, err := filepathHash(file.path); err == nil {
			expected := sha256.Sum256(file.data)
			if actual != fmt.Sprintf("%x", expected) {
				return "", errors.New("archive content conflict")
			}
			continue
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if err := privatefile.AtomicFile(file.path, file.data, 0600); err != nil {
			return "", err
		}
	}
	return path, nil
}
