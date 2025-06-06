package tenant

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidTenantIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *string
	}{
		{
			name: "tenant-a",
		},
		{
			name: "ABCDEFGHIJKLMNOPQRSTUVWXYZ-abcdefghijklmnopqrstuvwxyz_0987654321!.*'()",
		},
		{
			name: "invalid|",
			err:  strptr("tenant ID 'invalid|' contains unsupported character '|'"),
		},
		{
			name: strings.Repeat("a", 150),
		},
		{
			name: strings.Repeat("a", 151),
			err:  strptr("tenant ID is too long: max 150 characters"),
		},
		{
			name: ".",
			err:  strptr("tenant ID is '.' or '..'"),
		},
		{
			name: "..",
			err:  strptr("tenant ID is '.' or '..'"),
		},
		{
			name: "__markers__",
			err:  strptr("tenant ID '__markers__' is not allowed"),
		},
		{
			name: "user-index.json.gz",
			err:  strptr("tenant ID 'user-index.json.gz' is not allowed"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidTenantID(tc.name)
			if tc.err == nil {
				assert.Nil(t, err)
			} else {
				assert.EqualError(t, err, *tc.err)
			}
		})
	}
}
