package metric

import "testing"

func TestSeriesKeyOf(t *testing.T) {
	key := func(labels map[string]string) string {
		t.Helper()
		k, err := seriesKeyOf(labels)
		if err != nil {
			t.Fatalf("seriesKeyOf(%v): %v", labels, err)
		}
		return k
	}

	tests := []struct {
		name  string
		a, b  map[string]string
		equal bool
	}{
		{"nil and empty are one series", nil, map[string]string{}, true},
		{"key order does not matter", map[string]string{"a": "1", "b": "2"}, map[string]string{"b": "2", "a": "1"}, true},
		{"separator inside a value does not collide", map[string]string{"a": "1,b=2"}, map[string]string{"a": "1", "b": "2"}, false},
		{"equals inside a key does not collide", map[string]string{"a=1": ""}, map[string]string{"a": "1="}, false},
		{"different values differ", map[string]string{"mountpoint": "/"}, map[string]string{"mountpoint": "/boot"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := key(tt.a) == key(tt.b); got != tt.equal {
				t.Errorf("seriesKeyOf(%v)=%q, seriesKeyOf(%v)=%q; equal=%v, want %v",
					tt.a, key(tt.a), tt.b, key(tt.b), got, tt.equal)
			}
		})
	}
}
