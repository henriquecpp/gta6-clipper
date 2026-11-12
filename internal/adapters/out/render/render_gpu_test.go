//go:build gpu

package render

import "testing"

// TestGPUPreviewNVENC renders a preview with the real NVENC encoder.
func TestGPUPreviewNVENC(t *testing.T) {
	if r := renderPreview(t, EncoderNVENC); !r.NVENC() {
		t.Fatal("h264_nvenc not available in the container (check the `video` capability)")
	}
}
