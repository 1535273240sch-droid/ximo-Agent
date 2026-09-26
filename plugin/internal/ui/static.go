package ui

import (
	"embed"
	"fmt"
	"io/fs"
)

// assetsFS 把界面静态资源整包嵌进二进制（契约 §1）：运行时零外部请求，不加载任何
// CDN / 网络字体 / 远程图片。
//
// 用 `assets/*`（不含 all: 前缀）与契约 §5 一致：以 `.` 或 `_` 开头的文件不参与嵌入，
// 所以别把必需资源命名成 `_x.css`。
//
//go:embed assets/*
var assetsFS embed.FS

// assetDir 是内嵌目录名。资源归 U-Frontend 所有（internal/ui/assets/**），
// 这里只负责把它们端出去。
const assetDir = "assets"

// assetRoot 是去掉目录前缀后的资源文件系统（index.html / app.css / app.js …）。
var assetRoot = mustSub(assetsFS, assetDir)

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(fmt.Sprintf("ui: 内嵌资源目录 %s 不可用: %v", dir, err))
	}
	return sub
}

// assetFS 返回静态资源文件系统，供 http.FileServerFS 使用。
func assetFS() fs.FS { return assetRoot }

// readAsset 读一个内嵌资源。
func readAsset(name string) ([]byte, error) { return fs.ReadFile(assetRoot, name) }
