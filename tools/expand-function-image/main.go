// expand-function-image is a build-only tool. It never enters the Guest image.
package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
)

func expand(source, destination string, expected int64) (err error) {
	if expected <= 0 || expected > 16<<30 { return errors.New("invalid expected image size") }
	in, err := os.Open(source); if err != nil { return err }; defer in.Close()
	reader, err := gzip.NewReader(in); if err != nil { return err }; defer reader.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600); if err != nil { return err }; defer out.Close()
	buffer, zeros := make([]byte, 1<<20), make([]byte, 1<<20); var total int64
	for {
		n, readErr := reader.Read(buffer)
		if total+int64(n) > expected { return errors.New("image exceeds declared size") }
		if n > 0 {
			if bytes.Equal(buffer[:n], zeros[:n]) { _, err = out.Seek(int64(n), io.SeekCurrent) } else { var written int; written, err = out.Write(buffer[:n]); if err == nil && written != n { err = io.ErrShortWrite } }
			if err != nil { return err }; total += int64(n)
		}
		if readErr == io.EOF { break }; if readErr != nil { return readErr }
	}
	// Reading through EOF verifies gzip CRC/length before creating a carrier.
	if total != expected { return errors.New("image size differs from declaration") }
	if err = out.Truncate(total); err != nil { return err }; return out.Sync()
}
func main() {
	if len(os.Args) != 4 { fmt.Fprintln(os.Stderr,"Usage: expand-function-image INPUT.gz NEW.raw EXACT_BYTES"); os.Exit(2) }
	expected, err := strconv.ParseInt(os.Args[3],10,64); if err == nil { err = expand(os.Args[1],os.Args[2],expected) }
	if err != nil { fmt.Fprintln(os.Stderr,"Sparse expansion failed:",err); os.Exit(1) }
}
