/*
   Velociraptor - Hunting Evil
   Copyright (C) 2019 Velocidex Innovations.

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published
   by the Free Software Foundation, either version 3 of the License, or
   (at your option) any later version.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <https://www.gnu.org/licenses/>.
*/
// A filesystem accessor for accessing registry hives through raw
// file parsing.

// We make the registry look like a filesystem:
// 1. Keys are mapped as directories, and values are files.
// 2. The file is interpreted as a URL with the following format:
//    accessor:/path#key_path
// 3. We use the accessor and path to open the underlying file, then
//    extract the key or value named by the key_path from it.
// 4. Normalized paths contain / for directory separators.
// 5. Normalized paths have rawreg: prefix.
package filesystem

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	errors "github.com/pkg/errors"
	"www.velocidex.com/golang/regparser"
	"www.velocidex.com/golang/velociraptor/glob"
	"www.velocidex.com/golang/vfilter"
)

const (
	MAX_EMBEDDED_REG_VALUE = 4 * 1024
)

type RawRegKeyInfo struct {
	key        *regparser.CM_KEY_NODE
	_full_path string
}

func (self *RawRegKeyInfo) IsDir() bool {
	return true
}

func (self *RawRegKeyInfo) Data() interface{} {
	return vfilter.NewDict().Set("type", "Key")
}

func (self *RawRegKeyInfo) Size() int64 {
	return 0
}

func (self *RawRegKeyInfo) Sys() interface{} {
	return nil
}

func (self *RawRegKeyInfo) FullPath() string {
	return self._full_path
}

func (self *RawRegKeyInfo) Mode() os.FileMode {
	return 0755 | os.ModeDir
}

func (self *RawRegKeyInfo) Name() string {
	return self.key.Name()
}

func (self *RawRegKeyInfo) ModTime() time.Time {
	return self.key.LastWriteTime().Time
}

func (self *RawRegKeyInfo) Mtime() glob.TimeVal {
	nsec := self.ModTime().UnixNano()
	return glob.TimeVal{
		Sec:  nsec / 1000000000,
		Nsec: nsec,
	}
}

func (self *RawRegKeyInfo) Ctime() glob.TimeVal {
	return self.Mtime()
}

func (self *RawRegKeyInfo) Atime() glob.TimeVal {
	return self.Mtime()
}

// Not supported
func (self *RawRegKeyInfo) IsLink() bool {
	return false
}

func (self *RawRegKeyInfo) GetLink() (string, error) {
	return "", errors.New("Not implemented")
}

func (self RawRegKeyInfo) MarshalJSON() ([]byte, error) {
	result, err := json.Marshal(&struct {
		FullPath string
		Data     interface{}
		Mtime    glob.TimeVal
		Ctime    glob.TimeVal
		Atime    glob.TimeVal
	}{
		FullPath: self.FullPath(),
		Mtime:    self.Mtime(),
		Ctime:    self.Ctime(),
		Atime:    self.Atime(),
		Data:     self.Data(),
	})

	return result, err
}

func (u *RawRegKeyInfo) UnmarshalJSON(data []byte) error {
	return nil
}

type RawRegValueInfo struct {
	// Containing key
	*RawRegKeyInfo
	value *regparser.CM_KEY_VALUE
}

func (self *RawRegValueInfo) Name() string {
	return self.value.ValueName()
}

func (self *RawRegValueInfo) Sys() interface{} {
	return self.value.ValueData()
}

func (self *RawRegValueInfo) IsDir() bool {
	return false
}

func (self *RawRegValueInfo) Mode() os.FileMode {
	return 0755
}

func (self *RawRegValueInfo) Size() int64 {
	return int64(self.value.DataSize())
}

func (self *RawRegValueInfo) Data() interface{} {
	value_data := self.value.ValueData()
	result := vfilter.NewDict().
		Set("type", self.value.TypeString()).
		Set("data_len", len(value_data.Data))

	switch value_data.Type {
	case regparser.REG_SZ, regparser.REG_MULTI_SZ, regparser.REG_EXPAND_SZ:
		result.Set("data", strings.TrimRight(value_data.String, "\x00"))

	case regparser.REG_DWORD, regparser.REG_QWORD, regparser.REG_DWORD_BIG_ENDIAN:
		result.Set("data", value_data.Uint64)
	default:
		if len(value_data.Data) < MAX_EMBEDDED_REG_VALUE {
			result.Set("data", value_data.Data)
		}
	}
	return result
}

func (self RawRegValueInfo) MarshalJSON() ([]byte, error) {
	result, err := json.Marshal(&struct {
		FullPath string
		Type     string
		Data     interface{}
		Mtime    glob.TimeVal
		Ctime    glob.TimeVal
		Atime    glob.TimeVal
	}{
		FullPath: self.FullPath(),
		Mtime:    self.Mtime(),
		Ctime:    self.Ctime(),
		Atime:    self.Atime(),
		Type:     self.value.TypeString(),
		Data:     self.Data(),
	})

	return result, err
}

type RawValueBuffer struct {
	*bytes.Reader
	info *RawRegValueInfo
}

func (self *RawValueBuffer) Stat() (os.FileInfo, error) {
	return self.info, nil
}

func (self *RawValueBuffer) Close() error {
	return nil
}

func NewRawValueBuffer(buf string, stat *RawRegValueInfo) *RawValueBuffer {
	return &RawValueBuffer{
		bytes.NewReader(stat.value.ValueData().Data),
		stat,
	}
}

type RawRegistryFileCache struct {
	registry *regparser.Registry
	fd       glob.ReadSeekCloser
}

type RawRegFileSystemAccessor struct {
	mu       sync.Mutex
	fd_cache map[string]*RawRegistryFileCache
}

func (self *RawRegFileSystemAccessor) GetRegHive(
	file_path string) (*RawRegistryFileCache, *url.URL, error) {
	url, err := url.Parse(file_path)
	if err != nil {
		return nil, nil, err
	}
	accessor := glob.GetAccessor(url.Scheme, context.Background())
	_, isnull := accessor.(glob.NullFileSystemAccessor)
	if accessor == nil || isnull {
		return nil, nil, errors.New("Unknown delegate accessor")
	}

	self.mu.Lock()
	defer self.mu.Unlock()

	base_url := *url
	base_url.Fragment = ""

	file_cache, pres := self.fd_cache[base_url.String()]
	if !pres {
		fd, err := accessor.Open(url.Path)
		if err != nil {
			return nil, nil, err
		}

		reader, ok := fd.(io.ReaderAt)
		if !ok {
			return nil, nil, errors.New("file is not seekable")
		}

		registry, err := regparser.NewRegistry(reader)
		if err != nil {
			return nil, nil, err
		}

		file_cache = &RawRegistryFileCache{
			registry: registry, fd: fd}

		self.fd_cache[url.String()] = file_cache
	}

	return file_cache, url, nil
}

func (self *RawRegFileSystemAccessor) New(
	ctx context.Context) glob.FileSystemAccessor {
	result := &RawRegFileSystemAccessor{
		fd_cache: make(map[string]*RawRegistryFileCache),
	}

	// When the context is done, close all the files.
	go func() {
		select {
		case <-ctx.Done():
			result.mu.Lock()
			defer result.mu.Unlock()

			for _, v := range result.fd_cache {
				v.fd.Close()
			}

			result.fd_cache = make(
				map[string]*RawRegistryFileCache)
		}
	}()

	return result
}

func (self RawRegFileSystemAccessor) ReadDir(key_path string) ([]glob.FileInfo, error) {
	var result []glob.FileInfo

	file_cache, url, err := self.GetRegHive(key_path)
	if err != nil {
		return nil, err
	}

	key := file_cache.registry.OpenKey(url.Fragment)
	if key == nil {
		return nil, errors.New("Key not found")
	}

	for _, subkey := range key.Subkeys() {
		result = append(result,
			&RawRegKeyInfo{
				subkey,
				self.PathJoin(key_path, subkey.Name()),
			})
	}

	for _, value := range key.Values() {
		result = append(result,
			&RawRegValueInfo{
				&RawRegKeyInfo{
					key,
					self.PathJoin(
						key_path, value.ValueName()),
				}, value,
			})
	}

	return result, nil
}

func (self RawRegFileSystemAccessor) Open(path string) (glob.ReadSeekCloser, error) {
	return nil, errors.New("Not implemented")
}

func (self *RawRegFileSystemAccessor) Lstat(filename string) (glob.FileInfo, error) {
	return nil, errors.New("Not implemented")
}

func (self *RawRegFileSystemAccessor) GetRoot(path string) (string, string, error) {
	url, err := url.Parse(path)
	if err != nil {
		return "", "", err
	}

	fragment := url.Fragment
	url.Fragment = ""

	return url.String() + "#", fragment, nil
}

// We accept both / and \ as a path separator
func (self *RawRegFileSystemAccessor) PathSplit(path string) []string {
	return regparser.SplitComponents(path)
}

func (self *RawRegFileSystemAccessor) PathJoin(root, stem string) string {
	// If any of the subsequent components contain
	// a slash then escape them together.
	if strings.Contains(stem, "/") {
		stem = "\"" + stem + "\""
	}

	url, err := url.Parse(root)
	if err != nil {
		fmt.Printf("Error %v Joining %v and %v -> %v\n",
			err, root, stem, path.Join(root, stem))
		return path.Join(root, stem)
	}

	url.Fragment = path.Join(url.Fragment, stem)

	result := url.String()

	return result
}

func init() {
	glob.Register("raw_reg", &RawRegFileSystemAccessor{})
}
