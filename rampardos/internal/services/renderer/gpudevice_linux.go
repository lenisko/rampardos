package renderer

import (
	"os"

	"maplibre.org/maplibre-native-ffi/bindings/go/maplibre"
)

// gpuDeviceOptions picks the backend explicitly: the library defaults to
// Vulkan whenever a Vulkan driver is installed, so adding Mesa's lavapipe
// to the image would otherwise switch backends silently.
// RENDERER_GPU_BACKEND=vulkan selects Vulkan (spike: for benchmarking).
func gpuDeviceOptions() *maplibre.GPUDeviceOptions {
	if os.Getenv("RENDERER_GPU_BACKEND") == "vulkan" {
		b := maplibre.RenderBackendVulkan
		return &maplibre.GPUDeviceOptions{Backend: &b}
	}
	b := maplibre.RenderBackendOpenGL
	gl := maplibre.OpenGLClientAPIGL
	return &maplibre.GPUDeviceOptions{Backend: &b, OpenGLClientAPI: &gl}
}
