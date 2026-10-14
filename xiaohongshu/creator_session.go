package xiaohongshu

import (
	"time"

	"github.com/go-rod/rod"
	"github.com/pkg/errors"
)

// 移植自上游 #855：发布前先建立创作者中心会话。

const (
	urlOfCreatorHome = `https://creator.xiaohongshu.com/?source=official`

	creatorSessionCookie  = "galaxy_creator_session_id"
	creatorSessionTimeout = 15 * time.Second
)

// ensureCreatorSession 浏览器里没有创作者中心会话时，先打开创作者中心首页，等会话建立
func ensureCreatorSession(page *rod.Page) error {
	if hasCreatorSession(page) {
		return nil
	}

	if err := page.Navigate(urlOfCreatorHome); err != nil {
		return errors.Wrap(err, "打开创作者中心失败")
	}

	deadline := time.Now().Add(creatorSessionTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		if hasCreatorSession(page) {
			time.Sleep(1 * time.Second) // 会话建立后稍作停顿再跳发布页
			return nil
		}
	}
	return errors.New("创作者中心登录失效，请重新扫码登录")
}

func hasCreatorSession(page *rod.Page) bool {
	cks, err := page.Browser().GetCookies()
	if err != nil {
		return false
	}
	for _, c := range cks {
		if c.Name == creatorSessionCookie {
			return true
		}
	}
	return false
}
