// Copyright 2023 Chainguard, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package impl

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha1" // nolint:gosec // this is what apk tools is using
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// writeOneFile writes one file from the APK given the tar header and tar reader.
func (a *APKImplementation) writeOneFile(header *tar.Header, r io.Reader) error {
	f, err := a.fs.OpenFile(header.Name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, header.FileInfo().Mode())
	if err != nil {
		return fmt.Errorf("error creating file %s: %w", header.Name, err)
	}
	defer f.Close()

	if _, err := io.CopyN(f, r, header.Size); err != nil {
		return fmt.Errorf("unable to write content for %s: %w", header.Name, err)
	}
	// override one of the
	return nil
}

// installAPKFiles install the files from the APK and return the list of installed files
// and their permissions. Returns a tar.Header because it is a convenient existing
// struct that has all of the fields we need.
func (a *APKImplementation) installAPKFiles(gzipIn io.Reader) ([]tar.Header, error) {
	var files []tar.Header
	gr, err := gzip.NewReader(gzipIn)
	if err != nil {
		return nil, err
	}
	// per https://git.alpinelinux.org/apk-tools/tree/src/extract_v2.c?id=337734941831dae9a6aa441e38611c43a5fd72c0#n120
	//  * APKv1.0 compatibility - first non-hidden file is
	//  * considered to start the data section of the file.
	//  * This does not make any sense if the file has v2.0
	//  * style .PKGINFO
	var startedDataSection bool
	tr := tar.NewReader(gr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		// if it was a hidden file and not a directory and we have not yet started the data section,
		// so skip this file
		if !startedDataSection && header.Name[0] == '.' && !strings.Contains(header.Name, "/") {
			continue
		}
		// whatever it is now, it is in the data section
		startedDataSection = true

		switch header.Typeflag {
		case tar.TypeDir:
			// special case, if the target already exists, and it is a symlink to a directory, we can accept it as is
			// otherwise, we need to create the directory.
			if fi, err := a.fs.Stat(header.Name); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				if target, err := a.fs.Readlink(header.Name); err == nil {
					if fi, err = a.fs.Stat(target); err == nil && fi.IsDir() {
						// "break" rather than "continue", so that any handling outside of this switch statement is processed
						break
					}
				}
			}
			if err := a.fs.MkdirAll(header.Name, header.FileInfo().Mode().Perm()); err != nil {
				return nil, fmt.Errorf("error creating directory %s: %w", header.Name, err)
			}
		case tar.TypeReg:
			// we need to calculate the checksum of the file while reading it
			w := sha1.New() //nolint:gosec // this is what apk tools is using
			tee := io.TeeReader(tr, w)
			if err := a.writeOneFile(header, tee); err != nil {
				return nil, err
			}
			// it uses this format
			checksum := fmt.Sprintf("Q1%s", base64.StdEncoding.EncodeToString(w.Sum(nil)))
			// we need to save this somewhere. The output expects []tar.Header, so we need to override that.
			// Reusing a field should be good enough, provided that we know it is not getting in the way of
			// anything downstream. Since we know it is not, this is good enough.
			if header.PAXRecords == nil {
				header.PAXRecords = make(map[string]string)
			}
			header.PAXRecords[paxRecordsChecksumKey] = checksum
		case tar.TypeSymlink:
			// some underlying filesystems and some memfs that we use in tests do not support symlinks.
			// attempt it, and if it fails, just copy it.
			if err := a.fs.Symlink(header.Linkname, header.Name); err != nil {
				return nil, err
			}
		case tar.TypeLink:
			if err := a.fs.Link(header.Linkname, header.Name); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unsupported file type %v", header.Typeflag)
		}
		files = append(files, *header)
	}

	return files, nil
}
