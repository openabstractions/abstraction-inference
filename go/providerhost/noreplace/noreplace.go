// Package noreplace moves a finished file into place only when nothing
// stands at its name, so a writer that builds a file under a temporary name
// never replaces one another program or an earlier run put there. It is the
// rename `openabstractions models pack` uses and the provider kit's video
// encoder uses (inference CONTRACT.md INF-VE1): MoveFileW without
// MOVEFILE_REPLACE_EXISTING on Windows, renameat2(RENAME_NOREPLACE) on Linux,
// renamex_np(RENAME_EXCL) on macOS, and a hard link where a filesystem
// refuses those. An existing target is refused with an error that wraps
// fs.ErrExist.
package noreplace
