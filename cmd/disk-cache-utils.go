/*
 * MinIO Cloud Storage, (C) 2019 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio/cmd/crypto"
)

type cacheControl struct {
	expiry   time.Time
	maxAge   int
	sMaxAge  int
	minFresh int
	maxStale int
}

func (c cacheControl) isEmpty() bool {
	return c == cacheControl{}

}

func (c cacheControl) isStale(modTime time.Time) bool {
	if c.isEmpty() {
		return false
	}
	now := time.Now()

	if c.sMaxAge > 0 && c.sMaxAge < int(now.Sub(modTime).Seconds()) {
		return true
	}
	if c.maxAge > 0 && c.maxAge < int(now.Sub(modTime).Seconds()) {
		return true
	}

	if !c.expiry.Equal(time.Time{}) && c.expiry.Before(time.Now().Add(time.Duration(c.maxStale))) {
		return true
	}

	if c.minFresh > 0 && c.minFresh <= int(now.Sub(modTime).Seconds()) {
		return true
	}

	return false
}

// returns struct with cache-control settings from user metadata.
func cacheControlOpts(o ObjectInfo) (c cacheControl) {
	m := o.UserDefined
	if o.Expires != timeSentinel {
		c.expiry = o.Expires
	}

	var headerVal string
	for k, v := range m {
		if strings.ToLower(k) == "cache-control" {
			headerVal = v
		}

	}
	if headerVal == "" {
		return
	}
	headerVal = strings.ToLower(headerVal)
	headerVal = strings.TrimSpace(headerVal)

	vals := strings.Split(headerVal, ",")
	for _, val := range vals {
		val = strings.TrimSpace(val)
		p := strings.Split(val, "=")

		if len(p) != 2 {
			continue
		}
		if p[0] == "max-age" ||
			p[0] == "s-maxage" ||
			p[0] == "min-fresh" ||
			p[0] == "max-stale" {
			i, err := strconv.Atoi(p[1])
			if err != nil {
				return cacheControl{}
			}
			if p[0] == "max-age" {
				c.maxAge = i
			}
			if p[0] == "s-maxage" {
				c.sMaxAge = i
			}
			if p[0] == "min-fresh" {
				c.minFresh = i
			}
			if p[0] == "max-stale" {
				c.maxStale = i
			}
		}
	}
	return c
}

// backendDownError returns true if err is due to backend failure or faulty disk if in server mode
func backendDownError(err error) bool {
	_, backendDown := err.(BackendDown)
	return backendDown || IsErr(err, baseErrs...)
}

// IsCacheable returns if the object should be saved in the cache.
func (o ObjectInfo) IsCacheable() bool {
	return !crypto.IsEncrypted(o.UserDefined)
}

// reads file cached on disk from offset upto length
func readCacheFileStream(filePath string, offset, length int64) (io.ReadCloser, error) {
	if filePath == "" || offset < 0 {
		return nil, errInvalidArgument
	}
	if err := checkPathLength(filePath); err != nil {
		return nil, err
	}

	fr, err := os.Open(filePath)
	if err != nil {
		return nil, osErrToFSFileErr(err)
	}
	// Stat to get the size of the file at path.
	st, err := fr.Stat()
	if err != nil {
		err = osErrToFSFileErr(err)
		return nil, err
	}

	// Verify if its not a regular file, since subsequent Seek is undefined.
	if !st.Mode().IsRegular() {
		return nil, errIsNotRegular
	}

	if err = os.Chtimes(filePath, time.Now(), st.ModTime()); err != nil {
		return nil, err
	}

	// Seek to the requested offset.
	if offset > 0 {
		_, err = fr.Seek(offset, io.SeekStart)
		if err != nil {
			return nil, err
		}
	}
	return struct {
		io.Reader
		io.Closer
	}{Reader: io.LimitReader(fr, length), Closer: fr}, nil
}
