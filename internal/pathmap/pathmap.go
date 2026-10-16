package pathmap

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// AmbiguousWorkspaceError is returned when multiple directories match the target project name.
type AmbiguousWorkspaceError struct {
	BasePrefix  string
	ProjectName string
	Candidates  []string
}

func (e *AmbiguousWorkspaceError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ambiguous target directory: multiple matches for %q found under %q:\n", e.ProjectName, e.BasePrefix)
	for _, c := range e.Candidates {
		fmt.Fprintf(&b, "  - %s\n", c)
	}
	b.WriteString("please specify the intended path explicitly with --target-path <path>")
	return b.String()
}

// IsAmbiguousError reports whether err is or wraps *AmbiguousWorkspaceError.
func IsAmbiguousError(err error) bool {
	var target *AmbiguousWorkspaceError
	return errors.As(err, &target)
}

// ExpandPath expands a leading "~" or "~/" using the current user's home directory.
func ExpandPath(p string) string {
	if p == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// ProjectName extracts the canonical project folder name from cwd.
func ProjectName(cwd string) string {
	cleaned := filepath.Clean(ExpandPath(cwd))
	return filepath.Base(cleaned)
}

var ignoredWalkDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".venv":        true,
	"venv":         true,
	"target":       true,
	"dist":         true,
	"build":        true,
	".cache":       true,
	".next":        true,
}

const maxSearchDepth = 3

// ResolveWorkspaceDir searches under basePrefix for directories named projectName.
//   - 0 matches: returns filepath.Join(basePrefix, projectName), false, nil
//   - 1 match:   returns matchedPath, true, nil
//   - >1 matches: returns "", false, *AmbiguousWorkspaceError
func ResolveWorkspaceDir(basePrefix, projectName string) (string, bool, error) {
	root := filepath.Clean(ExpandPath(basePrefix))
	if projectName == "" || projectName == "." || projectName == "/" {
		return "", false, fmt.Errorf("invalid project name %q", projectName)
	}

	st, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return filepath.Join(root, projectName), false, nil
		}
		return "", false, fmt.Errorf("checking base prefix %s: %w", root, err)
	}
	if !st.IsDir() {
		return "", false, fmt.Errorf("base prefix %s is not a directory", root)
	}

	var matches []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Skip unreadable subdirectories cleanly.
			if d != nil && d.IsDir() && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() || path == root {
			return nil
		}

		name := d.Name()
		if strings.HasPrefix(name, ".") && name != projectName {
			return filepath.SkipDir
		}
		if ignoredWalkDirs[name] && name != projectName {
			return filepath.SkipDir
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		depth := strings.Count(rel, string(os.PathSeparator)) + 1

		if name == projectName {
			matches = append(matches, path)
			// Do not recurse inside a matched project directory to avoid nested self-matches.
			return filepath.SkipDir
		}

		if depth >= maxSearchDepth {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return "", false, fmt.Errorf("searching %s for %s: %w", root, projectName, err)
	}

	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return filepath.Join(root, projectName), false, nil
	case 1:
		return matches[0], true, nil
	default:
		return "", false, &AmbiguousWorkspaceError{
			BasePrefix:  root,
			ProjectName: projectName,
			Candidates:  matches,
		}
	}
}

// PathToFileURI converts an absolute file path to a file:// URI.
func PathToFileURI(absPath string) string {
	cleaned := filepath.ToSlash(filepath.Clean(absPath))
	if !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return "file://" + cleaned
}

// FileURIToPath converts a file:// URI to a local filesystem path.
func FileURIToPath(uri string) string {
	trimmed := strings.TrimPrefix(strings.TrimSpace(uri), "file://")
	if trimmed == "" {
		return ""
	}
	return filepath.Clean(filepath.FromSlash(trimmed))
}

// RewriteWorkspaceURIsJSON rewrites workspace_uris JSON arrays (e.g. ["file:///old/path"])
// replacing oldPath with newPath (or setting [file:///newPath] if non-empty).
func RewriteWorkspaceURIsJSON(rawJSON, oldWorkspace, newWorkspace string) string {
	if newWorkspace == "" {
		return rawJSON
	}
	newURI := PathToFileURI(newWorkspace)
	oldURI := ""
	if oldWorkspace != "" {
		oldURI = PathToFileURI(oldWorkspace)
	}

	var uris []string
	if err := json.Unmarshal([]byte(rawJSON), &uris); err != nil || len(uris) == 0 {
		out, _ := json.Marshal([]string{newURI})
		return string(out)
	}

	replaced := false
	for i, u := range uris {
		if oldURI != "" && u == oldURI {
			uris[i] = newURI
			replaced = true
		}
	}
	if !replaced {
		uris[0] = newURI
	}
	out, err := json.Marshal(uris)
	if err != nil {
		return rawJSON
	}
	return string(out)
}

// RewriteRolloutFirstLine updates payload.cwd and payload.runtime_workspace_roots in the first JSONL line of a Codex rollout file.
func RewriteRolloutFirstLine(line []byte, newCwd string) ([]byte, error) {
	if newCwd == "" {
		return line, nil
	}
	trimmed := bytes.TrimRight(line, "\r\n")
	if len(trimmed) == 0 {
		return line, nil
	}

	var doc map[string]any
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return nil, fmt.Errorf("parsing rollout header JSON: %w", err)
	}

	if payload, ok := doc["payload"].(map[string]any); ok {
		oldCwd, _ := payload["cwd"].(string)
		payload["cwd"] = newCwd

		if roots, ok := payload["runtime_workspace_roots"].([]any); ok {
			newRoots := make([]string, 0, len(roots))
			replaced := false
			for _, r := range roots {
				rs, _ := r.(string)
				if oldCwd != "" && rs == oldCwd {
					newRoots = append(newRoots, newCwd)
					replaced = true
				} else if rs != "" {
					newRoots = append(newRoots, rs)
				}
			}
			if !replaced {
				newRoots = []string{newCwd}
			}
			payload["runtime_workspace_roots"] = newRoots
		} else {
			payload["runtime_workspace_roots"] = []string{newCwd}
		}
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encoding rollout header JSON: %w", err)
	}
	return encoded, nil
}

// RewriteProtobufURIs traverses a Protocol Buffers wire-encoded blob and replaces any length-delimited
// string field equal to oldURI with newURI, recalculating all parent message length varints.
func RewriteProtobufURIs(blob []byte, oldURI, newURI string) []byte {
	if len(blob) == 0 || oldURI == "" || newURI == "" || oldURI == newURI {
		return blob
	}
	if !bytes.Contains(blob, []byte(oldURI)) {
		return blob
	}
	out, ok := rewriteProtoMessage(blob, []byte(oldURI), []byte(newURI))
	if !ok {
		return blob
	}
	return out
}

func rewriteProtoMessage(data, oldBytes, newBytes []byte) ([]byte, bool) {
	var out bytes.Buffer
	pos := 0
	for pos < len(data) {
		tag, n := binary.Uvarint(data[pos:])
		if n <= 0 {
			return nil, false
		}
		fieldNum := tag >> 3
		wireType := tag & 0x7
		if fieldNum == 0 {
			return nil, false
		}
		tagBytes := data[pos : pos+n]
		pos += n

		switch wireType {
		case 0: // Varint
			_, vn := binary.Uvarint(data[pos:])
			if vn <= 0 {
				return nil, false
			}
			out.Write(tagBytes)
			out.Write(data[pos : pos+vn])
			pos += vn
		case 1: // 64-bit
			if pos+8 > len(data) {
				return nil, false
			}
			out.Write(tagBytes)
			out.Write(data[pos : pos+8])
			pos += 8
		case 5: // 32-bit
			if pos+4 > len(data) {
				return nil, false
			}
			out.Write(tagBytes)
			out.Write(data[pos : pos+4])
			pos += 4
		case 2: // Length-delimited
			length, ln := binary.Uvarint(data[pos:])
			if ln <= 0 {
				return nil, false
			}
			pos += ln
			if uint64(len(data)-pos) < length {
				return nil, false
			}
			payload := data[pos : pos+int(length)]
			pos += int(length)

			var newPayload []byte
			if bytes.Equal(payload, oldBytes) {
				newPayload = newBytes
			} else if bytes.Contains(payload, oldBytes) {
				if nested, ok := rewriteProtoMessage(payload, oldBytes, newBytes); ok {
					newPayload = nested
				} else {
					newPayload = payload
				}
			} else {
				newPayload = payload
			}

			out.Write(tagBytes)
			var lenBuf [binary.MaxVarintLen64]byte
			lnOut := binary.PutUvarint(lenBuf[:], uint64(len(newPayload)))
			out.Write(lenBuf[:lnOut])
			out.Write(newPayload)
		default:
			return nil, false
		}
	}
	return out.Bytes(), true
}
