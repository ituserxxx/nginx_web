//go:build embed

package main

import "embed"

//go:embed dist
var webrootFS embed.FS

// embedWebroot 标记本构建是否内嵌了前端产物（由 build.sh 通过 -tags embed 打开）
const embedWebroot = true
