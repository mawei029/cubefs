package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"reflect"

	"gopkg.in/go-playground/validator.v9"
)

const (
	_ESCAPE   = 92 /* \ */
	_QUOTE    = 34 /* " */
	_HASH     = 35 /* # */
	_SPACE    = 32 /* space  */
	_TAB      = 9  /* tab */
	_LF       = 10 /* \n */
	_ASTERISK = 42 /* * */
	_SLASH    = 47 /* / */
)

var validate *validator.Validate

func init() {
	validate = validator.New()
	validate.RegisterValidation("required_with_parent", requiredWithParent)
}

func requiredWithParent(fl validator.FieldLevel) bool {
	parent := fl.Parent()
	if !isZero(parent) {
		return !isZero(fl.Field())
	}
	return true
}

func LoadData(conf interface{}, data []byte) (err error) {
	data = trimComments(data)

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err = dec.Decode(conf)
	if err != nil {
		log.Println("Parse conf failed:", err)
		return
	}

	err = validate.Struct(conf)
	if err != nil {
		if _, ok := err.(*validator.InvalidValidationError); ok {
			return nil
		}
		err = fmt.Errorf("validate failed: %v", err)
		log.Println("LoadData failed:", err)
	}
	return
}

// support json5 comment
func trimComments(s []byte) []byte {
	var (
		quote             bool
		escaped           bool
		commentStart      bool
		multiCommentLine  bool
		singleCommentLine bool
	)

	start := func(c byte) {
		commentStart = true
		multiCommentLine = c == _ASTERISK
		singleCommentLine = (c == _SLASH || c == _HASH)
	}
	stop := func() {
		commentStart = false
		multiCommentLine = false
		singleCommentLine = false
	}

	n := len(s)
	if n == 0 {
		return s
	}
	str := make([]byte, 0, n)

	for i := 0; i < n; i++ {
		if s[i] == _ESCAPE || escaped {
			if !commentStart {
				str = append(str, s[i])
			}
			escaped = !escaped
			continue
		}
		if s[i] == _QUOTE {
			quote = !quote
		}
		if (s[i] == _SPACE || s[i] == _TAB) && !quote {
			continue
		}
		if s[i] == _LF {
			if singleCommentLine {
				stop()
			}
			continue
		}
		if quote && !commentStart {
			str = append(str, s[i])
			continue
		}
		if commentStart {
			if multiCommentLine && s[i] == _ASTERISK && i+1 < n && s[i+1] == _SLASH {
				stop()
				i++
			}
			continue
		}

		if s[i] == _HASH {
			start(_HASH)
			continue
		}

		if s[i] == _SLASH && i+1 < n {
			if s[i+1] == _ASTERISK {
				start(_ASTERISK)
				i++
				continue
			}

			if s[i+1] == _SLASH {
				start(_SLASH)
				i++
				continue
			}
		}
		str = append(str, s[i])
	}
	return str
}

// isZero is a func for checking whether value is zero
func isZero(field reflect.Value) bool {
	switch field.Kind() {
	case reflect.Ptr:
		if field.IsNil() {
			return true
		}
		return isZero(field.Elem())

	case reflect.Interface, reflect.Chan, reflect.Func:
		return field.IsNil()

	case reflect.Slice, reflect.Map:
		return field.Len() == 0

	case reflect.Array:
		for i, n := 0, field.Len(); i < n; i++ {
			fl := field.Index(i)
			if !isZero(fl) {
				return false
			}
		}
		return true

	case reflect.Struct:
		for i, n := 0, field.NumField(); i < n; i++ {
			fl := field.Field(i)
			if !isZero(fl) {
				return false
			}
		}
		return true

	default:
		if !field.IsValid() {
			return true
		}
		return reflect.DeepEqual(field.Interface(), reflect.Zero(field.Type()).Interface())
	}
}
