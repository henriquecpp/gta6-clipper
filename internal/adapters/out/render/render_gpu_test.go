//go:build gpu

package render

import "testing"

// TestGPUPreviewNVENC renders a preview with the real NVENC encoder.
func TestGPUPreviewNVENC(t *testing.T) {
	if r := renderPreview(t, EncoderNVENC); !r.NVENC() {
		t.Fatal("h264_nvenc not available in the container (check the `video` capability)")
	}
}

// TestGPUFinalNVENC renders the final 9:16 video with the real NVENC encoder.
func TestGPUFinalNVENC(t *testing.T) {
	if r := renderFinal(t, EncoderNVENC, "blur"); !r.NVENC() {
		t.Fatal("h264_nvenc not available")
	}
}
