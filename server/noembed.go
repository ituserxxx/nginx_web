//go:build !embed

package main

import "io/fs"

// 未启用 embed 构建（开发模式 / 常规单测）时，不内嵌前端，webrootFS 置空，
// registerWebroot 会因为 embedWebroot=false 直接返回，不影响后端 /api 行为。
var webrootFS fs.FS

// embedWebroot 标记本构建是否内嵌了前端产物
const embedWebroot = false
