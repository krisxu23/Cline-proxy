package kit

import (
	"os"
	"path/filepath"
)

// WriteFileAtomic 原子写文件: 先写同目录临时文件, fsync 后 rename 覆盖目标。
//
// 直接 os.WriteFile 覆写的问题是它先截断再写: 进程在写到一半时被强杀
// (或断电) 会留下一个被截断的半个 JSON, 下次启动解析失败 —— 账号池与
// 各类配置会静默丢失。rename 在同一文件系统内是原子的, 所以读者要么
// 看到旧文件, 要么看到完整的新文件, 不存在中间态。
//
// 同目录是刻意的: 跨文件系统的 rename 会退化成复制, 不再原子。
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// 任何一步失败都要清掉临时文件, 否则会在 data/ 下积一堆残留。
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// 先落盘再改名: 否则崩溃后可能出现"文件名已更新但内容还在页缓存里"。
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// Windows 上 rename 不保证目录项立即持久化, 但已足够抵御进程被杀;
	// 真正的掉电语义需要目录 fsync, Go 未跨平台暴露, 不强行实现。
	return nil
}

// WriteFileAtomicDefault 是 WriteFileAtomic 的常用权限封装(0600)。
// 这些文件里含 API Key / refreshToken, 一律不对外开放读权限。
func WriteFileAtomicDefault(path string, data []byte) error {
	return WriteFileAtomic(path, data, 0o600)
}
