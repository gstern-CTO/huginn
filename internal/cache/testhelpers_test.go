package cache

import "os"

// writeFileForTest is a small helper so tests can create fixture files without
// importing os everywhere.
func writeFileForTest(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o644)
}
