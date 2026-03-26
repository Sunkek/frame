package frame

import (
	"fmt"
	"strings"
)

// multiError holds one or more errors and formats them as a single message.
// It supports errors.Is/As unwrapping over the full list.
type multiError []error

func (e multiError) Error() string {
	if len(e) == 1 {
		return e[0].Error()
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d errors occurred:", len(e)))
	for _, err := range e {
		b.WriteString("\n  - ")
		b.WriteString(err.Error())
	}
	return b.String()
}

func (e multiError) Unwrap() []error { return []error(e) }
