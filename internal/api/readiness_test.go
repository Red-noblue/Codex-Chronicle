// 中文说明：验证发布门槛报告中的资源单位不会把 GiB 误显示为 TiB。
package api

import "testing"

func TestFormatBytesUsesCorrectBinaryUnits(t *testing.T) {
	tests := []struct {
		name  string
		value int64
		want  string
	}{
		{name: "bytes", value: 512, want: "512 B"},
		{name: "kibibytes", value: 1 << 10, want: "1.0 KB"},
		{name: "gibibytes", value: 2 << 30, want: "2.0 GB"},
		{name: "tebibytes", value: 3 << 40, want: "3.0 TB"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := formatBytes(test.value); got != test.want {
				t.Fatalf("formatBytes(%d) = %q, want %q", test.value, got, test.want)
			}
		})
	}
}
