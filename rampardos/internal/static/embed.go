package static

import "embed"

//go:embed css/*.css js/*.js webfonts/*.woff2
var FS embed.FS
