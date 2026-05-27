package xiaohongshu

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/proto"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// PublishImageContent 发布图文内容
type PublishImageContent struct {
	Title           string
	Content         string
	Tags            []string
	ImagePaths      []string
	ScheduleTime    *time.Time // 定时发布时间，nil 表示立即发布
	IsOriginal      bool       // 是否声明原创
	Visibility      string     // 可见范围: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
	Products        []string   // 商品关键词列表，用于绑定带货商品
	GroupChat       string     // 群聊名；空=不绑（必绑：配了绑不上→发布失败）
	BindLivePreview bool       // true=关联最近一场未来直播预告（best-effort）
}

type PublishAction struct {
	page *rod.Page
}

const (
	urlOfPublic = `https://creator.xiaohongshu.com/publish/publish?source=official`
)

func NewPublishImageAction(page *rod.Page) (*PublishAction, error) {

	pp := page.Timeout(300 * time.Second)

	// 使用更稳健的导航和等待策略
	if err := pp.Navigate(urlOfPublic); err != nil {
		return nil, errors.Wrap(err, "导航到发布页面失败")
	}

	// 等待页面加载，使用 WaitLoad 代替 WaitIdle（更宽松）
	if err := pp.WaitLoad(); err != nil {
		logrus.Warnf("等待页面加载出现问题: %v，继续尝试", err)
	}
	time.Sleep(2 * time.Second)

	// 等待页面稳定
	if err := pp.WaitDOMStable(time.Second, 0.1); err != nil {
		logrus.Warnf("等待 DOM 稳定出现问题: %v，继续尝试", err)
	}
	time.Sleep(1 * time.Second)

	if err := mustClickPublishTab(pp, "上传图文"); err != nil {
		logrus.Errorf("点击上传图文 TAB 失败: %v", err)
		return nil, err
	}

	time.Sleep(1 * time.Second)

	return &PublishAction{
		page: pp,
	}, nil
}

func (p *PublishAction) Publish(ctx context.Context, content PublishImageContent) error {
	if len(content.ImagePaths) == 0 {
		return errors.New("图片不能为空")
	}

	page := p.page.Context(ctx)

	if err := uploadImages(page, content.ImagePaths); err != nil {
		return errors.Wrap(err, "小红书上传图片失败")
	}

	tags := content.Tags
	if len(tags) >= 10 {
		logrus.Warnf("标签数量超过10，截取前10个标签")
		tags = tags[:10]
	}

	logrus.Infof("发布内容: title=%s, images=%v, tags=%v, schedule=%v, original=%v, visibility=%s, products=%v", content.Title, len(content.ImagePaths), tags, content.ScheduleTime, content.IsOriginal, content.Visibility, content.Products)

	if err := submitPublish(page, content.Title, content.Content, tags, content.ScheduleTime, content.IsOriginal, content.Visibility, content.Products, content.GroupChat, content.BindLivePreview); err != nil {
		return errors.Wrap(err, "小红书发布失败")
	}

	return nil
}

func removePopCover(page *rod.Page) {

	// 先移除弹窗封面
	has, elem, err := page.Has("div.d-popover")
	if err != nil {
		return
	}
	if has {
		elem.MustRemove()
	}

	// 兜底：点击一下空位置吧
	clickEmptyPosition(page)
}

func clickEmptyPosition(page *rod.Page) {
	x := 380 + rand.Intn(100)
	y := 20 + rand.Intn(60)
	page.Mouse.MustMoveTo(float64(x), float64(y)).MustClick(proto.InputMouseButtonLeft)
}

func mustClickPublishTab(page *rod.Page, tabname string) error {
	page.MustElement(`div.upload-content`).MustWaitVisible()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		tab, blocked, err := getTabElement(page, tabname)
		if err != nil {
			logrus.Warnf("获取发布 TAB 元素失败: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if tab == nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if blocked {
			logrus.Info("发布 TAB 被遮挡，尝试移除遮挡")
			removePopCover(page)
			time.Sleep(200 * time.Millisecond)
			continue
		}

		if err := tab.Click(proto.InputMouseButtonLeft, 1); err != nil {
			logrus.Warnf("点击发布 TAB 失败: %v", err)
			time.Sleep(200 * time.Millisecond)
			continue
		}

		return nil
	}

	return errors.Errorf("没有找到发布 TAB - %s", tabname)
}

func getTabElement(page *rod.Page, tabname string) (*rod.Element, bool, error) {
	elems, err := page.Elements("div.creator-tab")
	if err != nil {
		return nil, false, err
	}

	for _, elem := range elems {
		if !isElementVisible(elem) {
			continue
		}

		text, err := elem.Text()
		if err != nil {
			logrus.Debugf("获取发布 TAB 文本失败: %v", err)
			continue
		}

		if strings.TrimSpace(text) != tabname {
			continue
		}

		blocked, err := isElementBlocked(elem)
		if err != nil {
			return nil, false, err
		}

		return elem, blocked, nil
	}

	return nil, false, nil
}

func isElementBlocked(elem *rod.Element) (bool, error) {
	result, err := elem.Eval(`() => {
		const rect = this.getBoundingClientRect();
		if (rect.width === 0 || rect.height === 0) {
			return true;
		}
		const x = rect.left + rect.width / 2;
		const y = rect.top + rect.height / 2;
		const target = document.elementFromPoint(x, y);
		return !(target === this || this.contains(target));
	}`)
	if err != nil {
		return false, err
	}

	return result.Value.Bool(), nil
}

func uploadImages(page *rod.Page, imagesPaths []string) error {
	// 验证文件路径有效性
	validPaths := make([]string, 0, len(imagesPaths))
	for _, path := range imagesPaths {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			logrus.Warnf("图片文件不存在: %s", path)
			continue
		}
		validPaths = append(validPaths, path)
		logrus.Infof("获取有效图片：%s", path)
	}

	// 逐张上传：每张上传后等待预览出现，再上传下一张
	for i, path := range validPaths {
		selector := `input[type="file"]`
		if i == 0 {
			selector = ".upload-input"
		}

		uploadInput, err := page.Element(selector)
		if err != nil {
			return errors.Wrapf(err, "查找上传输入框失败(第%d张)", i+1)
		}
		if err := uploadInput.SetFiles([]string{path}); err != nil {
			return errors.Wrapf(err, "上传第%d张图片失败", i+1)
		}

		slog.Info("图片已提交上传", "index", i+1, "path", path)

		// 等待当前图片上传完成（预览元素数量达到 i+1），最多等 60 秒
		if err := waitForUploadComplete(page, i+1); err != nil {
			return errors.Wrapf(err, "第%d张图片上传超时", i+1)
		}
		time.Sleep(1 * time.Second)
	}

	return nil
}

// waitForUploadComplete 等待第 expectedCount 张图片上传完成，最多等 60 秒
func waitForUploadComplete(page *rod.Page, expectedCount int) error {
	maxWaitTime := 60 * time.Second
	checkInterval := 500 * time.Millisecond
	start := time.Now()
	lastLogCount := expectedCount - 1

	for time.Since(start) < maxWaitTime {
		uploadedImages, err := page.Elements(".img-preview-area .pr")
		if err != nil {
			time.Sleep(checkInterval)
			continue
		}

		currentCount := len(uploadedImages)
		// 数量变化时才打印，避免刷屏
		if currentCount != lastLogCount {
			slog.Info("等待图片上传", "current", currentCount, "expected", expectedCount)
			lastLogCount = currentCount
		}
		if currentCount >= expectedCount {
			slog.Info("图片上传完成", "count", currentCount)
			return nil
		}

		time.Sleep(checkInterval)
	}

	return errors.Errorf("第%d张图片上传超时(60s)，请检查网络连接和图片大小", expectedCount)
}

func submitPublish(page *rod.Page, title, content string, tags []string, scheduleTime *time.Time, isOriginal bool, visibility string, products []string, groupChat string, bindLivePreview bool) error {
	titleElem, err := page.Element("div.d-input input")
	if err != nil {
		return errors.Wrap(err, "查找标题输入框失败")
	}
	if err := titleElem.Input(title); err != nil {
		return errors.Wrap(err, "输入标题失败")
	}

	// 检查标题长度
	time.Sleep(500 * time.Millisecond)
	if err := checkTitleMaxLength(page); err != nil {
		return err
	}
	slog.Info("检查标题长度：通过")

	time.Sleep(1 * time.Second)

	contentElem, ok := getContentElement(page)
	if !ok {
		return errors.New("没有找到内容输入框")
	}
	if err := contentElem.Input(content); err != nil {
		return errors.Wrap(err, "输入正文失败")
	}
	if err := waitAndClickTitleInput(titleElem); err != nil {
		return err
	}
	if err := inputTags(contentElem, tags); err != nil {
		return err
	}

	time.Sleep(1 * time.Second)

	// 检查正文长度
	if err := checkContentMaxLength(page); err != nil {
		return err
	}
	slog.Info("检查正文长度：通过")

	// 处理定时发布
	if scheduleTime != nil {
		if err := setSchedulePublish(page, *scheduleTime); err != nil {
			return errors.Wrap(err, "设置定时发布失败")
		}
		slog.Info("定时发布设置完成", "schedule_time", scheduleTime.Format("2006-01-02 15:04"))
	}

	// 设置可见范围
	if err := setVisibility(page, visibility); err != nil {
		return errors.Wrap(err, "设置可见范围失败")
	}

	// 处理原创声明
	if isOriginal {
		if err := setOriginal(page); err != nil {
			slog.Warn("设置原创声明失败，继续发布", "error", err)
		} else {
			slog.Info("已声明原创")
		}
	}

	// 绑定商品
	if err := bindProducts(page, products); err != nil {
		return errors.Wrap(err, "绑定商品失败")
	}

	// 绑定群聊（必绑：配了 groupChat 但下拉没匹配到 → error → 整帖失败重试，与 bindProducts 一致）
	if err := bindGroupChat(page, groupChat); err != nil {
		return errors.Wrap(err, "绑定群聊失败")
	}

	// 绑定直播预告（best-effort：失败只 warning 不挡发布；无未来预告则内部静默跳过）
	if bindLivePreview {
		if err := bindLivePreviewComponent(page); err != nil {
			slog.Warn("绑定直播预告失败，继续发布", "error", err)
		}
	}

	if err := clickPublishButton(page); err != nil {
		return err
	}

	// 等 XHS 后端 confirm 发布成功：URL 跳转到 /publish/success 或 DOM 出现 .success-page。
	// 没等 confirmation 就 return 是历史 bug：mcp 返 200 但 XHS 后端可能拒（违规/审核/图片
	// fail），publisher 误判成功，DB 写错状态。30s 超时给 XHS 后端足够时间（实测 ~3s 内跳转）。
	if err := waitForPublishSuccess(page, 30*time.Second); err != nil {
		return errors.Wrap(err, "发布完成确认失败")
	}
	return nil
}

// waitForPublishSuccess 等 XHS 后端 confirm 发布成功。
// 用单次 JS Eval 原子读 URL + visible DOM + 失败 toast，避开 rod page.Element
// 默认 sleeper 永久阻塞（codex review must）。
//
// 成功 signal（任一即视为成功）：
//  1. URL 含 "/publish/success"（XHS SPA 跳转，最强信号）
//  2. URL 含 "/publish/publish?...published=true"（dump 证据 t10 出现，二次确认）
//  3. visible `.success-page .success-title` 含 "发布成功"（DOM fallback，要求 visible）
//
// 失败 signal（短路返回 error 不等 timeout）：
//
//	body 含 "发布失败/内容违规/审核未通过/上传失败/网络异常/请稍后再试" 任一字串
//
// 都没等到（默认 30s）→ 返回 timeout error，携带 last_url 给排查。
func waitForPublishSuccess(page *rod.Page, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	probe := `() => {
		const url = location.href;
		const titleEl = document.querySelector('.success-page .success-title');
		let titleVisible = false;
		if (titleEl) {
			const rect = titleEl.getBoundingClientRect();
			const style = window.getComputedStyle(titleEl);
			titleVisible = rect.width > 0 && rect.height > 0
				&& style.display !== 'none'
				&& style.visibility !== 'hidden';
		}
		const titleOk = titleVisible && ((titleEl.innerText || '').indexOf('发布成功') >= 0);
		const failTexts = ['发布失败','内容违规','审核未通过','上传失败','网络异常','请稍后再试'];
		const body = (document.body && document.body.innerText) || '';
		let failMatch = '';
		for (const t of failTexts) { if (body.indexOf(t) >= 0) { failMatch = t; break; } }
		return {
			url: url,
			urlSuccess: url.indexOf('/publish/success') >= 0,
			urlPublished: /\/publish\/publish.*published=true/.test(url),
			successDOM: titleOk,
			failText: failMatch
		};
	}`
	var lastURL string
	for time.Now().Before(deadline) {
		res, err := page.Eval(probe)
		if err == nil && res != nil {
			v := res.Value
			lastURL = v.Get("url").String()
			if ft := v.Get("failText").String(); ft != "" {
				return errors.Errorf("发布失败 detected: %s (url=%s)", ft, lastURL)
			}
			if v.Get("urlSuccess").Bool() {
				slog.Info("发布成功 confirmed (URL=/publish/success)", "url", lastURL)
				return nil
			}
			if v.Get("successDOM").Bool() {
				slog.Info("发布成功 confirmed (DOM visible)", "url", lastURL)
				return nil
			}
			if v.Get("urlPublished").Bool() {
				slog.Info("发布成功 confirmed (URL ?published=true)", "url", lastURL)
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return errors.Errorf("等待发布成功超时 %s (last_url=%s)", maxWait, lastURL)
}

type publishButton struct {
	elem     *rod.Element
	isWidget bool
}

func clickPublishButton(page *rod.Page) error {
	btn, err := waitForPublishButtonClickable(page, 15*time.Second)
	if err != nil {
		return err
	}

	if btn.isWidget {
		return clickPublishWidget(page, btn.elem)
	}

	if err := btn.elem.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击发布按钮失败")
	}
	return nil
}

// waitForPublishButtonClickable 等待新版 xhs-publish-btn 或旧版 button.bg-red 可点击。
func waitForPublishButtonClickable(page *rod.Page, maxWait time.Duration) (*publishButton, error) {
	interval := 1 * time.Second
	start := time.Now()
	var lastDisabledReason string

	slog.Info("开始等待发布按钮可点击")

	for time.Since(start) < maxWait {
		btn, disabledReason, err := findPublishButton(page)
		if err != nil {
			slog.Warn("查找发布按钮失败，继续等待", "error", err)
			time.Sleep(interval)
			continue
		}
		if btn != nil && disabledReason == "" {
			return btn, nil
		}
		if disabledReason != "" {
			lastDisabledReason = disabledReason
		}
		time.Sleep(interval)
	}

	if lastDisabledReason != "" {
		return nil, errors.Errorf("等待发布按钮可点击超时: %s", lastDisabledReason)
	}
	return nil, errors.New("等待发布按钮可点击超时")
}

func findPublishButton(page *rod.Page) (*publishButton, string, error) {
	widgets, err := page.Elements("xhs-publish-btn")
	if err != nil {
		return nil, "", errors.Wrap(err, "查找新版发布按钮失败")
	}

	for _, widget := range widgets {
		if !isElementVisible(widget) {
			continue
		}

		isPublish, err := widget.Attribute("is-publish")
		if err != nil {
			return nil, "", errors.Wrap(err, "读取新版发布按钮 is-publish 属性失败")
		}
		if isPublish != nil && *isPublish == "false" {
			continue
		}

		submitDisabled, err := widget.Attribute("submit-disabled")
		if err != nil {
			return nil, "", errors.Wrap(err, "读取新版发布按钮 submit-disabled 属性失败")
		}
		if submitDisabled != nil && *submitDisabled == "true" {
			return &publishButton{elem: widget, isWidget: true}, "新版发布按钮不可点击", nil
		}

		return &publishButton{elem: widget, isWidget: true}, "", nil
	}

	oldButtons, err := page.Elements(".publish-page-publish-btn button.bg-red")
	if err != nil {
		return nil, "", errors.Wrap(err, "查找旧版发布按钮失败")
	}

	for _, oldButton := range oldButtons {
		if !isElementVisible(oldButton) {
			continue
		}

		if disabled, err := oldButton.Attribute("disabled"); err != nil {
			return nil, "", errors.Wrap(err, "读取旧版发布按钮 disabled 属性失败")
		} else if disabled != nil {
			return &publishButton{elem: oldButton}, "旧版发布按钮 disabled", nil
		}

		if ariaDisabled, err := oldButton.Attribute("aria-disabled"); err != nil {
			return nil, "", errors.Wrap(err, "读取旧版发布按钮 aria-disabled 属性失败")
		} else if ariaDisabled != nil && *ariaDisabled == "true" {
			return &publishButton{elem: oldButton}, "旧版发布按钮 aria-disabled=true", nil
		}

		if cls, err := oldButton.Attribute("class"); err != nil {
			return nil, "", errors.Wrap(err, "读取旧版发布按钮 class 属性失败")
		} else if cls != nil && hasExactClass(*cls, "disabled") {
			return &publishButton{elem: oldButton}, "旧版发布按钮包含 disabled class", nil
		}

		return &publishButton{elem: oldButton}, "", nil
	}

	return nil, "", nil
}

func clickPublishWidget(page *rod.Page, widget *rod.Element) error {
	if err := widget.ScrollIntoView(); err != nil {
		return errors.Wrap(err, "滚动新版发布按钮到可视区域失败")
	}
	time.Sleep(200 * time.Millisecond)

	shape, err := widget.Shape()
	if err != nil {
		return errors.Wrap(err, "获取新版发布按钮位置失败")
	}
	if len(shape.Quads) == 0 {
		return errors.New("获取新版发布按钮位置失败: 无可点击区域")
	}

	quad := shape.Quads[0]
	minX, maxX := quad[0], quad[0]
	minY, maxY := quad[1], quad[1]
	for i := 0; i < quad.Len(); i++ {
		x := quad[i*2]
		y := quad[i*2+1]
		if x < minX {
			minX = x
		}
		if x > maxX {
			maxX = x
		}
		if y < minY {
			minY = y
		}
		if y > maxY {
			maxY = y
		}
	}

	x := minX + (maxX-minX)*0.65
	y := minY + (maxY-minY)/2
	if err := page.Mouse.MoveTo(proto.Point{X: x, Y: y}); err != nil {
		return errors.Wrap(err, "移动到新版发布按钮失败")
	}
	if err := page.Mouse.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击发布按钮失败")
	}
	return nil
}

// waitAndClickTitleInput 在填写正文后等待 1 秒并回点标题输入框，增强后续交互稳定性
func waitAndClickTitleInput(titleElem *rod.Element) error {
	slog.Info("正文填写完成，准备等待后回点标题输入框")
	time.Sleep(1 * time.Second)
	if err := titleElem.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "回点标题输入框失败")
	}
	slog.Info("已回点标题输入框，继续后续发布流程")
	return nil
}

// 检查标题是否超过最大长度
func checkTitleMaxLength(page *rod.Page) error {
	has, elem, err := page.Has(`div.title-container div.max_suffix`)
	if err != nil {
		return errors.Wrap(err, "检查标题长度元素失败")
	}

	// 元素不存在，说明标题没超长
	if !has {
		return nil
	}

	// 元素存在，说明标题超长
	titleLength, err := elem.Text()
	if err != nil {
		return errors.Wrap(err, "获取标题长度文本失败")
	}

	return makeMaxLengthError(titleLength)
}

func checkContentMaxLength(page *rod.Page) error {
	has, elem, err := page.Has(`div.edit-container div.length-error`)
	if err != nil {
		return errors.Wrap(err, "检查正文长度元素失败")
	}

	// 元素不存在，说明正文没超长
	if !has {
		return nil
	}

	// 元素存在，说明正文超长
	contentLength, err := elem.Text()
	if err != nil {
		return errors.Wrap(err, "获取正文长度文本失败")
	}

	return makeMaxLengthError(contentLength)
}

func makeMaxLengthError(elemText string) error {
	parts := strings.Split(elemText, "/")
	if len(parts) != 2 {
		return errors.Errorf("长度超过限制: %s", elemText)
	}

	currLen, maxLen := parts[0], parts[1]

	return errors.Errorf("当前输入长度为%s，最大长度为%s", currLen, maxLen)
}

// 查找内容输入框 - 使用Race方法处理两种样式
func getContentElement(page *rod.Page) (*rod.Element, bool) {
	var foundElement *rod.Element
	var found bool

	page.Race().
		Element("div.ql-editor").MustHandle(func(e *rod.Element) {
		foundElement = e
		found = true
	}).
		ElementFunc(func(page *rod.Page) (*rod.Element, error) {
			return findTextboxByPlaceholder(page)
		}).MustHandle(func(e *rod.Element) {
		foundElement = e
		found = true
	}).
		MustDo()

	if found {
		return foundElement, true
	}

	slog.Warn("no content element found by any method")
	return nil, false
}

func inputTags(contentElem *rod.Element, tags []string) error {
	if len(tags) == 0 {
		return nil
	}

	time.Sleep(1 * time.Second)

	for i := 0; i < 20; i++ {
		ka, err := contentElem.KeyActions()
		if err != nil {
			return errors.Wrap(err, "创建键盘操作失败")
		}
		if err := ka.Type(input.ArrowDown).Do(); err != nil {
			return errors.Wrap(err, "按下方向键失败")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ka, err := contentElem.KeyActions()
	if err != nil {
		return errors.Wrap(err, "创建键盘操作失败")
	}
	if err := ka.Press(input.Enter).Press(input.Enter).Do(); err != nil {
		return errors.Wrap(err, "按下回车键失败")
	}

	time.Sleep(1 * time.Second)

	for _, tag := range tags {
		tag = strings.TrimLeft(tag, "#")
		if err := inputTag(contentElem, tag); err != nil {
			return errors.Wrapf(err, "输入标签[%s]失败", tag)
		}
	}
	return nil
}

func inputTag(contentElem *rod.Element, tag string) error {
	if err := contentElem.Input("#"); err != nil {
		return errors.Wrap(err, "输入#失败")
	}
	time.Sleep(200 * time.Millisecond)

	for _, char := range tag {
		if err := contentElem.Input(string(char)); err != nil {
			return errors.Wrapf(err, "输入字符[%c]失败", char)
		}
		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(1 * time.Second)

	page := contentElem.Page()
	topicContainer, err := page.Element("#creator-editor-topic-container")
	if err != nil || topicContainer == nil {
		slog.Warn("未找到标签联想下拉框，直接输入空格", "tag", tag)
		return contentElem.Input(" ")
	}

	firstItem, err := topicContainer.Element(".item")
	if err != nil || firstItem == nil {
		slog.Warn("未找到标签联想选项，直接输入空格", "tag", tag)
		return contentElem.Input(" ")
	}

	if err := firstItem.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击标签联想选项失败")
	}
	slog.Info("成功点击标签联想选项", "tag", tag)
	time.Sleep(200 * time.Millisecond)

	time.Sleep(500 * time.Millisecond) // 等待标签处理完成
	return nil
}

func findTextboxByPlaceholder(page *rod.Page) (*rod.Element, error) {
	elements := page.MustElements("p")
	if elements == nil {
		return nil, errors.New("no p elements found")
	}

	// 查找包含指定placeholder的元素
	placeholderElem := findPlaceholderElement(elements, "输入正文描述")
	if placeholderElem == nil {
		return nil, errors.New("no placeholder element found")
	}

	// 向上查找textbox父元素
	textboxElem := findTextboxParent(placeholderElem)
	if textboxElem == nil {
		return nil, errors.New("no textbox parent found")
	}

	return textboxElem, nil
}

func findPlaceholderElement(elements []*rod.Element, searchText string) *rod.Element {
	for _, elem := range elements {
		placeholder, err := elem.Attribute("data-placeholder")
		if err != nil || placeholder == nil {
			continue
		}

		if strings.Contains(*placeholder, searchText) {
			return elem
		}
	}
	return nil
}

func findTextboxParent(elem *rod.Element) *rod.Element {
	currentElem := elem
	for i := 0; i < 5; i++ {
		parent, err := currentElem.Parent()
		if err != nil {
			break
		}

		role, err := parent.Attribute("role")
		if err != nil || role == nil {
			currentElem = parent
			continue
		}

		if *role == "textbox" {
			return parent
		}

		currentElem = parent
	}
	return nil
}

// isElementVisible 检查元素是否可见
func isElementVisible(elem *rod.Element) bool {

	// 检查是否有隐藏样式
	style, err := elem.Attribute("style")
	if err == nil && style != nil {
		styleStr := *style

		if strings.Contains(styleStr, "left: -9999px") ||
			strings.Contains(styleStr, "top: -9999px") ||
			strings.Contains(styleStr, "position: absolute; left: -9999px") ||
			strings.Contains(styleStr, "display: none") ||
			strings.Contains(styleStr, "visibility: hidden") ||
			strings.Contains(styleStr, "opacity: 1e-05") {
			return false
		}

		// 精确匹配 opacity: 0（不匹配 0.5、0.1 等）
		if strings.Contains(styleStr, "opacity: 0") {
			// 确认是 opacity: 0 而非 opacity: 0.x
			if matched, _ := regexp.MatchString(`opacity:\s*0(\s|;|$)`, styleStr); matched {
				return false
			}
		}
	}

	// 检查 aria-hidden 属性
	ariaHidden, err := elem.Attribute("aria-hidden")
	if err == nil && ariaHidden != nil && *ariaHidden == "true" {
		return false
	}

	// 检查 tabindex 属性（-1 表示不可聚焦，通常也意味着不可见）
	tabindex, err := elem.Attribute("tabindex")
	if err == nil && tabindex != nil && *tabindex == "-1" {
		// 结合检查是否有 active class 来判断是否是真正的隐藏
		class, _ := elem.Attribute("class")
		// 使用单词边界检查，避免匹配 "inactive" 等
		if class == nil || !hasExactClass(*class, "active") {
			// 不是激活状态的 -1 tabindex 元素，可能是隐藏的叠加层
			return false
		}
	}

	visible, err := elem.Visible()
	if err != nil {
		slog.Warn("无法获取元素可见性", "error", err)
		return true
	}

	return visible
}

// hasExactClass 检查 class 字符串是否包含指定的完整类名（单词边界匹配）
func hasExactClass(classStr, className string) bool {
	pattern := `\b` + regexp.QuoteMeta(className) + `\b`
	matched, _ := regexp.MatchString(pattern, classStr)
	return matched
}

// setVisibility 设置可见范围
// 支持: "公开可见"(默认), "仅自己可见", "仅互关好友可见"
func setVisibility(page *rod.Page, visibility string) error {
	if visibility == "" || visibility == "公开可见" {
		slog.Info("可见范围使用默认：公开可见")
		return nil
	}

	// 支持的选项校验
	supported := map[string]bool{"仅自己可见": true, "仅互关好友可见": true}
	if !supported[visibility] {
		return errors.Errorf("不支持的可见范围: %s，支持: 公开可见、仅自己可见、仅互关好友可见", visibility)
	}

	// 点击可见范围下拉框
	dropdown, err := page.Element("div.permission-card-wrapper div.d-select-content")
	if err != nil {
		return errors.Wrap(err, "查找可见范围下拉框失败")
	}
	if err := dropdown.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击可见范围下拉框失败")
	}
	time.Sleep(500 * time.Millisecond)

	// 在弹窗中查找并点击目标选项
	opts, err := page.Elements("div.d-options-wrapper div.d-grid-item div.custom-option")
	if err != nil {
		return errors.Wrap(err, "查找可见范围选项失败")
	}
	for _, opt := range opts {
		text, err := opt.Text()
		if err != nil {
			continue
		}
		if strings.Contains(text, visibility) {
			if err := opt.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return errors.Wrap(err, "选择可见范围失败")
			}
			slog.Info("已设置可见范围", "visibility", visibility)
			time.Sleep(200 * time.Millisecond)
			return nil
		}
	}
	return errors.Errorf("未找到可见范围选项: %s", visibility)
}

// setSchedulePublish 设置定时发布时间
func setSchedulePublish(page *rod.Page, t time.Time) error {
	// 1. 点击定时发布开关
	if err := clickScheduleSwitch(page); err != nil {
		return err
	}
	time.Sleep(800 * time.Millisecond)

	// 2. 设置日期时间
	if err := setDateTime(page, t); err != nil {
		return err
	}
	time.Sleep(500 * time.Millisecond)

	return nil
}

// clickScheduleSwitch 点击定时发布开关
func clickScheduleSwitch(page *rod.Page) error {
	switchElem, err := page.Element(".post-time-wrapper .d-switch")
	if err != nil {
		return errors.Wrap(err, "查找定时发布开关失败")
	}

	if err := switchElem.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击定时发布开关失败")
	}
	slog.Info("已点击定时发布开关")
	return nil
}

// setDateTime 设置日期时间
func setDateTime(page *rod.Page, t time.Time) error {
	dateTimeStr := t.Format("2006-01-02 15:04")

	input, err := page.Element(".date-picker-container input")
	if err != nil {
		return errors.Wrap(err, "查找日期时间输入框失败")
	}

	if err := input.SelectAllText(); err != nil {
		return errors.Wrap(err, "选择日期时间文本失败")
	}
	if err := input.Input(dateTimeStr); err != nil {
		return errors.Wrap(err, "输入日期时间失败")
	}
	slog.Info("已设置日期时间", "datetime", dateTimeStr)

	return nil
}

// setOriginal 设置原创声明
func setOriginal(page *rod.Page) error {
	// 根据小红书创作者页面的实际结构：
	// div.custom-switch-card 包含 span.has-tips 文本为"原创声明"
	// 开关是 div.d-switch 组件

	// 查找包含"原创声明"文本的 custom-switch-card
	switchCards, err := page.Elements("div.custom-switch-card")
	if err != nil {
		return errors.Wrap(err, "查找原创声明卡片失败")
	}

	for _, card := range switchCards {
		text, err := card.Text()
		if err != nil {
			continue
		}

		// 检查是否是原创声明卡片
		if !strings.Contains(text, "原创声明") {
			continue
		}

		// 找到原创声明卡片，查找其中的 d-switch
		switchElem, err := card.Element("div.d-switch")
		if err != nil {
			continue
		}

		// 检查开关是否已打开
		checked, err := switchElem.Eval(`() => {
			const input = this.querySelector('input[type="checkbox"]');
			return input ? input.checked : false;
		}`)
		if err != nil {
			continue
		}

		if checked.Value.Bool() {
			slog.Info("原创声明已开启")
			return nil
		}

		// 点击开关
		if err := switchElem.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return errors.Wrap(err, "点击原创声明开关失败")
		}

		time.Sleep(500 * time.Millisecond)

		// 处理原创声明确认弹窗
		if err := confirmOriginalDeclaration(page); err != nil {
			return errors.Wrap(err, "确认原创声明失败")
		}

		slog.Info("已开启原创声明")
		return nil
	}

	return errors.New("未找到原创声明选项")
}

// confirmOriginalDeclaration 处理原创声明确认弹窗
func confirmOriginalDeclaration(page *rod.Page) error {
	// 等待确认弹窗出现
	time.Sleep(800 * time.Millisecond)

	// 使用 JavaScript 直接处理弹窗，更可靠
	result, err := page.Eval(`
		() => {
			// 查找包含"原创声明须知"的 footer 区域
			const footers = document.querySelectorAll('div.footer');
			for (const footer of footers) {
				// 检查是否包含原创声明相关内容
				if (!footer.textContent.includes('原创声明须知')) {
					continue;
				}

				// 找到 checkbox 并勾选
				const checkbox = footer.querySelector('div.d-checkbox input[type="checkbox"]');
				if (checkbox && !checkbox.checked) {
					checkbox.click();
					console.log('已勾选原创声明须知 checkbox');
				}

				// 等待一下让按钮变为可用
				return 'found_footer';
			}
			return 'footer_not_found';
		}
	`)
	if err != nil {
		slog.Warn("执行查找弹窗脚本失败", "error", err)
	} else if result.Value.String() == "footer_not_found" {
		slog.Warn("未找到原创声明确认弹窗的 footer")
	}

	time.Sleep(500 * time.Millisecond)

	// 再次使用 JavaScript 点击声明原创按钮
	result2, err := page.Eval(`
		() => {
			const footers = document.querySelectorAll('div.footer');
			for (const footer of footers) {
				if (!footer.textContent.includes('声明原创')) {
					continue;
				}

				// 找到声明原创按钮
				const btn = footer.querySelector('button.custom-button');
				if (btn) {
					// 检查是否禁用
					if (btn.classList.contains('disabled') || btn.disabled) {
						// 尝试再次勾选 checkbox
						const checkbox = footer.querySelector('div.d-checkbox input[type="checkbox"]');
						if (checkbox && !checkbox.checked) {
							checkbox.click();
						}
						return 'button_disabled';
					}
					btn.click();
					return 'clicked';
				}
			}
			return 'button_not_found';
		}
	`)
	if err != nil {
		return errors.Wrap(err, "执行点击按钮脚本失败")
	}

	status := result2.Value.String()
	slog.Info("原创声明确认结果", "status", status)

	if status == "button_not_found" {
		return errors.New("未找到声明原创按钮")
	}
	if status == "button_disabled" {
		return errors.New("声明原创按钮仍处于禁用状态")
	}

	slog.Info("已成功点击声明原创按钮")
	time.Sleep(300 * time.Millisecond)

	return nil
}

// debugDumpEnabled 报告是否启用 DOM dump (ENV XHS_DUMP_DOM 非空).
// caller 用它在 DOM query 之前 short-circuit, 保证 env 关闭时彻底 no-op
// (不额外 query DOM, 不影响 production timing).
func debugDumpEnabled() bool {
	return os.Getenv("XHS_DUMP_DOM") != ""
}

// dumpDOMForDebug 调试用 — 把元素 outerHTML 写到 /app/data/dump_*.html,
// 仅当 debugDumpEnabled() 时启用. 用于排查 bind product 等 DOM 兼容问题.
// 通过 volume mount /app/data 持久化到 host 的 xhs-data/, 容器重启不丢.
//
// elFn 是 closure: 只有 env 开启时才被调用 → 关闭时不会 query DOM 或触发 MustElement panic.
func dumpDOMForDebug(label, keyword string, elFn func() *rod.Element) {
	if !debugDumpEnabled() {
		return
	}
	el := elFn()
	if el == nil {
		slog.Info("[DOM DUMP] skip (nil element)", "label", label, "keyword", keyword)
		return
	}
	html, err := el.HTML()
	if err != nil {
		slog.Error("[DOM DUMP] read HTML failed", "label", label, "keyword", keyword, "error", err)
		return
	}
	safeKw := strings.ReplaceAll(keyword, "/", "_")
	// UnixNano 防同秒多次 dump 覆盖 (multi-product bind 时一秒会有 4+ dump).
	fn := filepath.Join("/app/data", fmt.Sprintf("dump_%s_%s_%d.html", label, safeKw, time.Now().UnixNano()))
	if err := os.WriteFile(fn, []byte(html), 0o644); err != nil {
		slog.Error("[DOM DUMP] write failed", "label", label, "keyword", keyword, "file", fn, "error", err)
		return
	}
	slog.Info("[DOM DUMP] saved", "label", label, "keyword", keyword, "file", fn, "bytes", len(html))
}

// bindProducts 绑定商品到发布内容
func bindProducts(page *rod.Page, products []string) error {
	if len(products) == 0 {
		return nil
	}

	slog.Info("开始绑定商品", "products", products)

	// 点击"添加商品"按钮
	if err := clickAddProductButton(page); err != nil {
		return errors.Wrap(err, "点击添加商品按钮失败")
	}
	time.Sleep(1 * time.Second)

	// 等待商品选择弹窗出现
	modal, err := waitForProductModal(page)
	if err != nil {
		return errors.Wrap(err, "等待商品弹窗失败")
	}
	slog.Info("商品选择弹窗已打开")

	// 遍历搜索并选择商品
	var failedProducts []string
	for _, keyword := range products {
		if err := searchAndSelectProduct(page, modal, keyword); err != nil {
			slog.Warn("搜索选择商品失败", "keyword", keyword, "error", err)
			failedProducts = append(failedProducts, keyword)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// 点击保存按钮
	slog.Info("准备点击保存按钮")
	if err := clickModalSaveButton(page, modal); err != nil {
		return errors.Wrap(err, "点击保存按钮失败")
	}
	slog.Info("保存按钮点击完成，开始等待弹窗关闭")

	// 等待弹窗关闭
	if err := waitForModalClose(page); err != nil {
		slog.Warn("等待弹窗关闭超时", "error", err)
	} else {
		slog.Info("弹窗已关闭")
	}

	if len(failedProducts) > 0 {
		return errors.Errorf("部分商品未找到: %v", failedProducts)
	}

	slog.Info("商品绑定完成", "total", len(products))
	time.Sleep(1000 * time.Millisecond)
	return nil
}

// clickAddProductButton 点击"添加商品"按钮
func clickAddProductButton(page *rod.Page) error {
	slog.Info("开始查找添加商品按钮")

	// 查找包含"添加商品"文本的元素
	spans, err := page.Elements("span.d-text")
	if err != nil {
		return errors.Wrap(err, "查找商品按钮文本失败")
	}

	for _, span := range spans {
		text, err := span.Text()
		if err != nil {
			continue
		}
		if strings.TrimSpace(text) == "添加商品" {
			slog.Info("找到添加商品文本，向上查找可点击父元素")
			// 向上查找可点击的父元素
			parent := span
			for i := 0; i < 5; i++ {
				p, err := parent.Parent()
				if err != nil {
					break
				}
				parent = p

				tagName, err := parent.Eval(`() => this.tagName.toLowerCase()`)
				if err != nil {
					continue
				}
				tag := tagName.Value.Str()

				// 检查是否为 button 或含 d-button class
				if tag == "button" {
					if err := parent.Click(proto.InputMouseButtonLeft, 1); err != nil {
						return errors.Wrap(err, "点击添加商品按钮失败")
					}
					slog.Info("已点击添加商品按钮")
					time.Sleep(300 * time.Millisecond) // 确保弹窗动画开始
					return nil
				}

				cls, _ := parent.Attribute("class")
				if cls != nil && strings.Contains(*cls, "d-button") {
					if err := parent.Click(proto.InputMouseButtonLeft, 1); err != nil {
						return errors.Wrap(err, "点击添加商品按钮失败")
					}
					slog.Info("已点击添加商品按钮")
					time.Sleep(300 * time.Millisecond) // 确保弹窗动画开始
					return nil
				}
			}
		}
	}

	return errors.New("未找到添加商品按钮，账号可能未开通商品功能")
}

// waitForProductModal 等待商品选择弹窗出现
func waitForProductModal(page *rod.Page) (*rod.Element, error) {
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		modal, err := page.Element(".multi-goods-selector-modal")
		if err == nil && modal != nil {
			visible, _ := modal.Visible()
			if visible {
				slog.Info("商品选择弹窗已出现")
				return modal, nil
			}
		}
		time.Sleep(100 * time.Millisecond) // 缩短轮询间隔，更快响应
	}

	return nil, errors.New("等待商品选择弹窗超时")
}

// searchAndSelectProduct 搜索并选择商品。
// 偶发 race：第一次搜某 keyword 后 checkbox lazy mount 慢，15s wait 超时。
// 同 modal 内重新 input + Enter 通常能 settle —— 失败时 retry 1 次。
// 不 reopen modal：log 证据显示 modal 引用稳定（后续 keyword 同 modal 内立即成功）。
func searchAndSelectProduct(page *rod.Page, modal *rod.Element, keyword string) error {
	const maxAttempts = 2
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			slog.Warn("搜索商品 retry",
				"keyword", keyword,
				"attempt", attempt,
				"prev_err", lastErr.Error())
			time.Sleep(2 * time.Second) // 等页面 settle
		}
		err := searchAndSelectProductOnce(page, modal, keyword)
		if err == nil {
			if attempt > 1 {
				slog.Info("搜索商品 retry 成功", "keyword", keyword, "attempt", attempt)
			}
			return nil
		}
		lastErr = err
	}
	return errors.Wrapf(lastErr, "搜索商品失败 (after %d attempts)", maxAttempts)
}

// searchAndSelectProductOnce 单次搜索 + 选中商品。原 searchAndSelectProduct 逻辑。
func searchAndSelectProductOnce(page *rod.Page, modal *rod.Element, keyword string) error {
	slog.Info("搜索商品", "keyword", keyword)

	// 1. 获取搜索框
	searchInput, err := modal.Element(`input[placeholder="搜索商品ID 或 商品名称"]`)
	if err != nil {
		return errors.Wrap(err, "未找到商品搜索框")
	}

	// 2. 清空并输入关键词（使用原生 JS setter + 完整事件）。
	// retry 路径：先显式 Click 确保 input 拿到 focus，避免 attempt 1 timeout 后
	// 焦点跑到别处导致 SelectAllText/Input 命中错的元素（codex review must）。
	if err := searchInput.Click(proto.InputMouseButtonLeft, 1); err != nil {
		slog.Warn("点击搜索框失败", "error", err)
	}
	if err := searchInput.SelectAllText(); err != nil {
		slog.Warn("选择搜索框文本失败", "error", err)
	}
	time.Sleep(100 * time.Millisecond)

	// 使用 rod Input 输入关键词
	if err := searchInput.Input(keyword); err != nil {
		return errors.Wrap(err, "输入搜索关键词失败")
	}
	time.Sleep(300 * time.Millisecond)

	// 3. 触发搜索（模拟键盘 Enter）
	if err := page.Keyboard.Press(input.Enter); err != nil {
		return errors.Wrap(err, "触发搜索失败")
	}

	// 4. 等待搜索结果加载
	time.Sleep(1 * time.Second)

	// 等待 loading 消失（使用与工作代码相同的选择器）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		loading, err := modal.Element(".goods-list-loading")
		if err != nil || loading == nil {
			break
		}
		visible, _ := loading.Visible()
		if !visible {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 等待商品列表渲染完成（使用与工作代码相同的选择器）
	for time.Now().Before(deadline) {
		productList, err := modal.Element(".goods-list-normal .good-card-container")
		if err == nil && productList != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond) // 额外等待确保渲染完成

	// [DEBUG INSTRUMENTATION] dump 搜索完成后的 goods-list-normal 全貌（看返回了几个 card,
	// 是否带 SKU 入口, 是否有 disabled 标识等）. closure 保证 env 关闭时不 query DOM.
	dumpDOMForDebug("after-search", keyword, func() *rod.Element {
		if el, _ := modal.Element(".goods-list-normal"); el != nil {
			return el
		}
		return modal
	})

	// 5. 点击第一个商品的 checkbox。XHS card 内部 checkbox 是 lazy 渲染，
	// 实测 5s 不够（fresh chrome + 完整商品名也失败），bump 到 15s。
	var checkbox *rod.Element
	checkboxDeadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(checkboxDeadline) {
		checkbox, err = modal.Element(".goods-list-normal .good-card-container .d-checkbox")
		if err == nil && checkbox != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil || checkbox == nil {
		// [DEBUG INSTRUMENTATION] checkbox 找不到时 dump 第一张商品卡，看结构差异.
		dumpDOMForDebug("no-checkbox-first-card", keyword, func() *rod.Element {
			card, _ := modal.Element(".goods-list-normal .good-card-container")
			return card
		})
		return errors.Wrap(err, "未找到商品选择框")
	}

	// [DEBUG INSTRUMENTATION] checkbox 找到时也 dump 一份做对照（成功/失败结构对比）.
	dumpDOMForDebug("found-checkbox-first-card", keyword, func() *rod.Element {
		card, _ := modal.Element(".goods-list-normal .good-card-container")
		return card
	})

	// 检查是否已经选中
	isChecked, err := checkbox.Eval(`(el) => {
		return el.querySelector('.d-checkbox-simulator.checked') !== null ||
			   el.querySelector('input[type="checkbox"]:checked') !== null;
	}`)
	if err == nil && isChecked.Value.Bool() {
		slog.Info("商品已选中，跳过", "keyword", keyword)
		return nil
	}

	if err := checkbox.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击商品选择框失败")
	}

	// 6. 随机延迟模拟人为操作（800-1500ms）
	randomDelay := 800 + rand.Intn(700)
	time.Sleep(time.Duration(randomDelay) * time.Millisecond)

	// [DEBUG INSTRUMENTATION] 点 checkbox 后 dump page body，看是否弹出 SKU 选规格子弹窗
	// （多 SKU 商品常见行为：点 checkbox 不直接勾，先弹规格选择）.
	// 用 Element (非 MustElement) 避免 env 关闭时仍可能 panic 的边角情况.
	dumpDOMForDebug("after-checkbox-click-body", keyword, func() *rod.Element {
		body, _ := page.Element("body")
		return body
	})

	slog.Info("已选择商品", "keyword", keyword)
	return nil
}

// clickModalSaveButton 点击保存按钮
func clickModalSaveButton(page *rod.Page, modal *rod.Element) error {
	// 查找保存按钮（参考工作代码：直接查找并点击，不强制要求找到）
	btn, err := modal.Element(".goods-selected-footer button")
	if err == nil && btn != nil {
		if err := btn.Click(proto.InputMouseButtonLeft, 1); err != nil {
			slog.Warn("点击保存按钮失败", "error", err)
		} else {
			slog.Info("已点击保存按钮")
			return nil
		}
	}

	// 尝试点击主按钮
	primaryBtn, err := modal.Element(".goods-selected-footer .d-button--primary")
	if err == nil && primaryBtn != nil {
		if err := primaryBtn.Click(proto.InputMouseButtonLeft, 1); err != nil {
			slog.Warn("点击主按钮失败", "error", err)
		} else {
			slog.Info("已点击主按钮")
			return nil
		}
	}

	slog.Warn("未找到保存按钮，继续执行")
	return nil
}

// waitForModalClose 等待弹窗关闭
func waitForModalClose(page *rod.Page) error {
	deadline := time.Now().Add(5 * time.Second)
	slog.Info("开始等待弹窗关闭")

	for time.Now().Before(deadline) {
		// 使用 Has 代替 Element，避免等待元素出现的阻塞
		has, _, err := page.Has(".multi-goods-selector-modal")
		if err != nil || !has {
			slog.Info("弹窗已关闭")
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}

	return errors.New("等待弹窗关闭超时")
}

// ============================================================ //
// 群聊 / 直播预告绑定（发布组件区）— 选择器来自 2026-05-27 spike 实测
// ============================================================ //

// bindGroupChat 绑定群聊（"添加组件"区「选择群聊」d-select）。仿 setVisibility 的 d-select 交互。
//
//	name == ""        → 不绑（skip，返回 nil）
//	name 非空但没匹配  → 返回 error（**必绑**：submitPublish 据此整帖失败重试，与 bindProducts 一致）
//
// spike 实测：trigger=.group-card-wrapper .d-select-content；选项=.d-options-wrapper
// .d-grid-item .custom-option，群名在 .group-info .name（同 setVisibility 选项选择器）。
func bindGroupChat(page *rod.Page, name string) error {
	if name == "" {
		return nil
	}
	slog.Info("开始绑定群聊", "name", name)

	// 点开群聊 d-select 下拉
	trigger, err := page.Element("div.group-card-wrapper div.d-select-content")
	if err != nil {
		return errors.Wrap(err, "查找群聊下拉框失败（账号可能无群聊组件）")
	}
	if err := trigger.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击群聊下拉框失败")
	}
	time.Sleep(800 * time.Millisecond) // 等下拉展开 + 群列表 lazy render

	// 遍历选项，按 .group-info .name 精确匹配（非群选项无 .group-info → 跳过）
	opts, err := page.Elements("div.d-options-wrapper div.d-grid-item div.custom-option")
	if err != nil {
		return errors.Wrap(err, "查找群聊选项失败")
	}
	var available []string
	for _, opt := range opts {
		nameEl, e := opt.Element("div.group-info div.name")
		if e != nil || nameEl == nil {
			continue
		}
		gname, e := nameEl.Text()
		if e != nil {
			continue
		}
		gname = strings.TrimSpace(gname)
		available = append(available, gname)
		if gname == name {
			if err := opt.Click(proto.InputMouseButtonLeft, 1); err != nil {
				return errors.Wrap(err, "选择群聊失败")
			}
			slog.Info("已选择群聊", "name", name)
			time.Sleep(300 * time.Millisecond)
			return nil
		}
	}
	return errors.Errorf("未找到群聊 %q（账号可选群: %v）", name, available)
}

// bindLivePreviewComponent 关联最近一场未来直播预告（"添加组件"区「关联直播预告」→ d-modal）。
// best-effort：调用方 submitPublish 对返回 error 只 warning、不挡发布。
//
//	无未来预告（列表空）→ 关弹窗 + 返回 nil（正常跳过，非错误）
//	有预告           → 按开播时间升序取最早一场，点其「关联预告」按钮，关弹窗
//
// spike 实测：入口=.live-preview-wrapper .setting-card；弹窗列表=.list-area .list-item；
// 开播时间=.item-left .header span（首个，格式 2006-01-02 15:04）；关联按钮=.item-right
// button「关联预告」；关闭=.d-modal-close。
func bindLivePreviewComponent(page *rod.Page) error {
	slog.Info("开始绑定直播预告")

	card, err := page.Element("div.live-preview-wrapper div.setting-card")
	if err != nil {
		return errors.Wrap(err, "查找直播预告入口失败")
	}
	if err := card.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击直播预告入口失败")
	}
	// 点开后所有返回路径都关弹窗，避免出错时弹窗遮挡后续 clickPublishButton（codex P2）。
	defer closeLivePreviewModal(page)

	// 等弹窗列表区出现（async 拉取预告）。用非阻塞 page.Has —— page.Element 默认 sleeper 会
	// retry 到 page 的 300s timeout，把"10s best-effort"拖成几分钟（codex P2）。
	var listArea *rod.Element
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if has, el, _ := page.Has("div.list-area"); has && el != nil {
			if visible, _ := el.Visible(); visible {
				listArea = el
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if listArea == nil {
		return errors.New("等待直播预告弹窗超时")
	}

	items, err := listArea.Elements("div.list-item")
	if err != nil {
		return errors.Wrap(err, "查找直播预告列表项失败")
	}
	if len(items) == 0 {
		slog.Info("无未来直播预告，跳过")
		return nil // defer 关弹窗
	}

	// 选最近一场（开播时间升序最早）；解析全失败则退化取第一项
	target, when := pickSoonestLiveItem(items)
	if target == nil {
		target = items[0]
	}
	slog.Info("准备关联直播预告", "candidates", len(items), "soonest", when)

	// 找该项的「关联预告」按钮（.item-right 内还有 .jump-btn「查看直播计划」，要按文本区分）
	btns, err := target.Elements("div.item-right button")
	if err != nil || len(btns) == 0 {
		return errors.New("未找到关联预告按钮")
	}
	var assoc *rod.Element
	for _, b := range btns {
		if t, _ := b.Text(); strings.Contains(t, "关联预告") {
			assoc = b
			break
		}
	}
	if assoc == nil {
		return errors.New("未找到「关联预告」按钮（可能已关联或文案变化）")
	}
	if err := assoc.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return errors.Wrap(err, "点击关联预告失败")
	}
	time.Sleep(800 * time.Millisecond)
	slog.Info("直播预告已关联", "soonest", when)
	return nil // defer 关弹窗
}

// pickSoonestLiveItem 从 .list-item 列表按开播时间（.item-left .header span 首个，
// 格式 2006-01-02 15:04）升序取最早。解析失败的项跳过；全失败返回 (nil, "")。
func pickSoonestLiveItem(items []*rod.Element) (*rod.Element, string) {
	var best *rod.Element
	var bestT time.Time
	var bestStr string
	for _, it := range items {
		span, e := it.Element("div.item-left div.header span")
		if e != nil || span == nil {
			continue
		}
		raw, e := span.Text()
		if e != nil {
			continue
		}
		raw = strings.TrimSpace(raw)
		t, e := time.Parse("2006-01-02 15:04", raw)
		if e != nil {
			continue
		}
		if best == nil || t.Before(bestT) {
			best, bestT, bestStr = it, t, raw
		}
	}
	return best, bestStr
}

// closeLivePreviewModal 点右上角 X 关弹窗，避免遮挡后续 clickPublishButton。失败仅 warning。
func closeLivePreviewModal(page *rod.Page) {
	x, err := page.Element("span.d-modal-close.d-clickable")
	if err != nil || x == nil {
		slog.Warn("未找到直播预告弹窗关闭按钮")
		return
	}
	if err := x.Click(proto.InputMouseButtonLeft, 1); err != nil {
		slog.Warn("关闭直播预告弹窗失败", "error", err)
		return
	}
	time.Sleep(300 * time.Millisecond)
}
