// Copyright 2018 Jump Trading
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

package influx

import (
	"bytes"
	"errors"
)

// NewTag creates a new Tag instance from key & value strings.
func NewTag(key, value string) Tag {
	return Tag{
		Key:   []byte(key),
		Value: []byte(value),
	}
}

// Tag represents a key/value pair (both bytes).
type Tag struct {
	Key   []byte
	Value []byte
}

// TagSet hold a number of Tag pairs. It implements sort.Interface.
type TagSet []Tag

func (t TagSet) Len() int           { return len(t) }
func (t TagSet) Swap(i, j int)      { t[i], t[j] = t[j], t[i] }
func (t TagSet) Less(i, j int) bool { return bytes.Compare(t[i].Key, t[j].Key) < 0 }

// ParseTags extracts the measurement name and tagset out of a
// line. The measurement name, tag key and tag values are
// unescaped. The remainder of the line (i.e. fields and timestamp) is
// also returned unchanged. Errors are returned if incorrectly
// formatted tags are present in the line.
func ParseTags(line []byte) ([]byte, TagSet, []byte, error) {
	measurement, line := Token(line, []byte(", "))

	if len(line) == 0 {
		// Measurement without anything else.
		return measurement, nil, nil, nil
	}

	var tags TagSet
	var key, value []byte
	for {
		if len(line) == 0 {
			return measurement, tags, nil, nil
		}
		if line[0] == ' ' {
			return measurement, tags, line[1:], nil
		}

		key, line = Token(line[1:], []byte("= ,"))
		if len(line) == 0 || line[0] != '=' {
			return nil, nil, nil, errors.New("invalid tag")
		}

		value, line = Token(line[1:], []byte(", "))
		if len(value) == 0 {
			return nil, nil, nil, errors.New("invalid tag")
		}

		tags = append(tags, Tag{
			Key:   key,
			Value: value,
		})
	}
}
