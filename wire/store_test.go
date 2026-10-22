package wire

import (
	"encoding/json"
	"testing"
)

func TestStoreOpFixtures(t *testing.T) {
	roundTrip[StoreOp](t, "store-op")
}

func TestStorePagesRoundTrip(t *testing.T) {
	op := string(fixtures(t, "store-op")["put.json"])
	cases := []struct {
		name string
		data string
		v    any
	}{
		{"ops", `{"ops":[` + op + `]}`, &StoreOps{}},
		{"apply", `{"accepted":[` + op + `],"head":3}`, &StoreApplyResult{}},
		{"pull", `{"ops":[` + op + `],"cursor":3,"more":false,"reset":true}`, &StorePull{}},
	}
	for _, c := range cases {
		if err := Decode([]byte(c.data), c.v); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		out, _ := json.Marshal(c.v)
		sameJSON(t, c.name, []byte(c.data), out)
	}
}
