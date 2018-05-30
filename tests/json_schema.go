// Licensed to Elasticsearch B.V. under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Elasticsearch B.V. licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package tests

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elastic/apm-server/processor"
	"github.com/elastic/apm-server/tests/loader"
	"github.com/elastic/beats/libbeat/common"
)

type ProcessorSetup struct {
	Proc processor.Processor
	// path to payload that should be a full and valid example
	FullPayloadPath string
	// path to ES template definitions
	TemplatePaths []string
}

type SchemaTestData struct {
	Key       string
	Valid     []interface{}
	Invalid   []Invalid
	Condition Condition
}
type Invalid struct {
	Msg    string
	Values []interface{}
}

type Condition struct {
	// If requirements for a field apply in case of anothers key absence,
	// add the key.
	Absence []string
	// If requirements for a field apply in case of anothers key specific values,
	// add the key and its values.
	Existence map[string]interface{}
}

type obj = map[string]interface{}

var (
	Str1024        = createStr(1024, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 _-")
	Str1024Special = createStr(1024, `⌘ `)
	Str1025        = createStr(1025, "")
)

// Test that payloads missing `required `attributes fail validation.
// - `required`: ensure required keys must not be missing or nil
// - `conditionally required`: prepare payload according to conditions, then
//   ensure required keys must not be missing
func (ps *ProcessorSetup) AttrsPresence(t *testing.T, requiredKeys *Set, condRequiredKeys map[string]Condition) {

	required := Union(requiredKeys, NewSet(
		"service",
		"service.name",
		"service.agent",
		"service.agent.name",
		"service.agent.version",
		"service.language.name",
		"service.runtime.name",
		"service.runtime.version",
		"service.framework.name",
		"service.framework.version",
		"process.pid",
	))

	payload, err := loader.LoadData(ps.FullPayloadPath)
	require.NoError(t, err)

	schemaKeys := NewSet()
	flattenJsonKeys(payload, "", schemaKeys)

	for _, k := range schemaKeys.Array() {
		key := k.(string)

		//test sending nil value for key
		ps.changePayload(t, key, nil, Condition{}, upsertFn,
			func(k string) (bool, string) {
				return !required.Contains(k), "but got null"
			},
		)

		//test removing key from payload
		cond, _ := condRequiredKeys[key]
		_, keyLast := splitKey(key)
		ps.changePayload(t, key, nil, cond, deleteFn,
			func(k string) (bool, string) {
				if required.Contains(k) {
					return false, fmt.Sprintf("missing properties: \"%s\"", keyLast)
				} else if _, ok := condRequiredKeys[k]; ok {
					return false, fmt.Sprintf("missing properties: \"%s\"", keyLast)
				}
				return true, ""
			},
		)
	}
}

// Test that field names indexed as `keywords` in Elasticsearch, have the same
// length limitation on the Intake API.
// APM Server has set all keyword restrictions to length 1024.
//
// keywordExceptionKeys: attributes defined as keywords in the ES template, but
//   do not require a length restriction in the json schema, e.g. due to regex
//   patterns defining a more specific restriction,
// templateToSchema: mapping for fields that are nested or named different on
//   ES level than on intake API
func (ps *ProcessorSetup) KeywordLimitation(t *testing.T, keywordExceptionKeys *Set, templateToSchema map[string]string) {

	// fetch keyword restricted field names from ES template
	keywordFields, err := fetchFlattenedFieldNames(ps.TemplatePaths, addKeywordFields)
	require.NoError(t, err)

	for _, k := range keywordFields.Array() {
		key := k.(string)

		if keywordExceptionKeys.Contains(key) {
			continue
		}
		for from, to := range templateToSchema {
			if strings.HasPrefix(key, from) {
				key = strings.Replace(key, from, to, 1)
				break
			}
		}

		testAttrs := func(val interface{}, valid bool, msg string) {
			ps.changePayload(t, key, val, Condition{}, upsertFn,
				func(k string) (bool, string) { return valid, msg })
		}

		testAttrs(createStr(1025, ""), false, "maxlength")
		testAttrs(createStr(1024, ""), true, "")
	}
}

// Test that specified values for attributes fail or pass
// the validation accordingly.
// The configuration and testing of valid attributes here is intended
// to ensure correct setup and configuration to avoid false negatives.
func (ps *ProcessorSetup) DataValidation(t *testing.T, testData []SchemaTestData) {
	for _, d := range testData {
		testAttrs := func(val interface{}, valid bool, msg string) {
			ps.changePayload(t, d.Key, val, d.Condition,
				upsertFn, func(k string) (bool, string) {
					return valid, msg
				})
		}

		for _, invalid := range d.Invalid {
			for _, v := range invalid.Values {
				testAttrs(v, false, invalid.Msg)
			}
		}
		for _, v := range d.Valid {
			testAttrs(v, true, "")
		}

	}
}

func (ps *ProcessorSetup) changePayload(
	t *testing.T,
	key string,
	val interface{},
	condition Condition,
	changeFn func(interface{}, string, interface{}) interface{},
	validateFn func(string) (bool, string),
) {

	// load payload
	payload, err := loader.LoadData(ps.FullPayloadPath)
	require.NoError(t, err)

	// prepare payload according to conditions:

	// - ensure specified keys being present
	for k, val := range condition.Existence {
		fnKey, keyToChange := splitKey(k)

		payload = iterateMap(payload, "", fnKey, keyToChange, val, upsertFn).(obj)
	}
	err = ps.Proc.Validate(payload)
	assert.NoError(t, err)

	// - ensure specified keys being absent
	for _, k := range condition.Absence {
		fnKey, keyToChange := splitKey(k)
		payload = iterateMap(payload, "", fnKey, keyToChange, nil, deleteFn).(obj)
	}

	// change payload for key to test
	fnKey, keyToChange := splitKey(key)
	payload = iterateMap(payload, "", fnKey, keyToChange, val, changeFn).(obj)

	// run actual validation
	err = ps.Proc.Validate(payload)
	if shouldValidate, errMsg := validateFn(key); shouldValidate {
		assert.NoError(t, err, fmt.Sprintf("Expected <%v> for key <%v> to be valid", val, key))
		_, err = ps.Proc.Decode(payload)
		assert.NoError(t, err)
	} else {
		if assert.Error(t, err) {
			assert.Contains(t, strings.ToLower(err.Error()), errMsg)
		}
	}
}

func createStr(n int, start string) string {
	buf := bytes.NewBufferString(start)
	for buf.Len() < n {
		buf.WriteString("a")
	}
	return buf.String()
}

func splitKey(s string) (string, string) {
	idx := strings.LastIndex(s, ".")
	if idx == -1 {
		return "", s
	}
	return s[:idx], s[idx+1:]
}

func upsertFn(m interface{}, k string, v interface{}) interface{} {
	fn := func(o obj, key string, val interface{}) obj { o[key] = val; return o }
	return applyFn(m, k, v, fn)
}

func deleteFn(m interface{}, k string, v interface{}) interface{} {
	fn := func(o obj, key string, _ interface{}) obj { delete(o, key); return o }
	return applyFn(m, k, v, fn)
}

func applyFn(m interface{}, k string, val interface{}, fn func(obj, string, interface{}) obj) interface{} {
	switch m.(type) {
	case obj:
		fn(m.(obj), k, val)
	case []interface{}:
		for _, e := range m.([]interface{}) {
			if eObj, ok := e.(obj); ok {
				fn(eObj, k, val)
			}
		}
	}
	return m
}

func iterateMap(m interface{}, prefix, fnKey, xKey string, val interface{}, fn func(interface{}, string, interface{}) interface{}) interface{} {
	if d, ok := m.(obj); ok {
		ma := d
		if prefix == "" && fnKey == "" {
			ma = fn(ma, xKey, val).(obj)
		}
		for k, v := range d {
			key := strConcat(prefix, k, ".")
			ma[k] = iterateMap(v, key, fnKey, xKey, val, fn)
			if key == fnKey {
				ma[k] = fn(ma[k], xKey, val)
			}
		}
		if len(ma) > 0 {
			return ma
		}
		return nil
	} else if d, ok := m.([]interface{}); ok {
		var ma []interface{}
		for _, i := range d {
			if r := iterateMap(i, prefix, fnKey, xKey, val, fn); r != nil {
				ma = append(ma, r)
			}
		}
		return ma
	} else {
		return m
	}
}

type Schema struct {
	Title                string
	Properties           map[string]*Schema
	AdditionalProperties obj
	PatternProperties    obj
	Items                *Schema
	MaxLength            int
}
type Mapping struct {
	from string
	to   string
}

func TestPayloadAttributesInSchema(t *testing.T, name string, undocumentedAttrs *Set, schema string) {
	payload, err := loader.LoadValidData(name)
	require.NoError(t, err)
	jsonNames := NewSet()
	flattenJsonKeys(payload, "", jsonNames)
	jsonNamesDoc := Difference(jsonNames, undocumentedAttrs)

	schemaStruct, _ := schemaStruct(strings.NewReader(schema))
	schemaNames := NewSet()
	flattenSchemaNames(schemaStruct, "", addAllPropNames, schemaNames)

	missing := Difference(jsonNamesDoc, schemaNames)
	if missing.Len() > 0 {
		msg := fmt.Sprintf("Json payload fields missing in Schema %v", missing)
		assert.Fail(t, msg)
	}

	missing = Difference(schemaNames, jsonNames)
	if missing.Len() > 0 {
		msg := fmt.Sprintf("Json schema fields missing in Payload %v", missing)
		assert.Fail(t, msg)
	}
}

func schemaStruct(reader io.Reader) (*Schema, error) {
	decoder := json.NewDecoder(reader)
	var schema Schema
	err := decoder.Decode(&schema)
	return &schema, err
}

func flattenSchemaNames(s *Schema, prefix string, addFn addProperty, flattened *Set) {
	if len(s.Properties) > 0 {
		for k, v := range s.Properties {
			flattenedKey := strConcat(prefix, k, ".")
			if addFn(v) {
				flattened.Add(flattenedKey)
			}
			flattenSchemaNames(v, flattenedKey, addFn, flattened)
		}
	} else if s.Items != nil {
		flattenSchemaNames(s.Items, prefix, addFn, flattened)
	}
}

type addProperty func(s *Schema) bool

func addAllPropNames(s *Schema) bool { return true }

func flattenJsonKeys(data interface{}, prefix string, flattened *Set) {
	if d, ok := data.(obj); ok {
		for k, v := range d {
			key := strConcat(prefix, k, ".")
			flattened.Add(key)
			flattenJsonKeys(v, key, flattened)
		}
	} else if d, ok := data.([]interface{}); ok {
		for _, v := range d {
			flattenJsonKeys(v, prefix, flattened)
		}
	}
}

func addKeywordFields(f common.Field) bool {
	if f.Type == "keyword" || f.ObjectType == "keyword" {
		return true
	} else if len(f.MultiFields) > 0 {
		for _, mf := range f.MultiFields {
			if mf.Type == "keyword" {
				return true
			}
		}
	}
	return false
}
