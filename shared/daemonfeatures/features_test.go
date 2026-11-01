package daemonfeatures

import (
	"reflect"
	"testing"
)

func TestEncodeParseRoundTrip(t *testing.T) {
	raw := Encode([]string{"example.alpha.v1", " Example.Beta.V1 ", "example.alpha.v1", "bad name!", ""})
	if raw != "example.alpha.v1,example.beta.v1" {
		t.Fatalf("Encode = %q", raw)
	}
	got := List(Parse(raw))
	if !reflect.DeepEqual(got, []string{"example.alpha.v1", "example.beta.v1"}) {
		t.Fatalf("Parse/List = %v", got)
	}
	if len(Parse("")) != 0 {
		t.Fatal("empty handshake must be the empty set (older daemon)")
	}
}

func TestParseCapsCount(t *testing.T) {
	var many []string
	for i := 0; i < MaxFeatures+10; i++ {
		many = append(many, "f"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if n := len(Parse(Encode(many))); n != MaxFeatures {
		t.Fatalf("kept %d features, want cap %d", n, MaxFeatures)
	}
}
