package xiaohongshu

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// matchTopicItemIndex 是话题选择器的核心：从下拉项文本里找与 tag 精确同名的项。
// 这些用例锁定「绝不把 tag 选成无关话题」的行为（修 #whoop→#Globall 一类误选）。

func TestMatchTopicItemIndex_ExactMatchOnOwnLine(t *testing.T) {
	// 名称与浏览量分行（最常见的 DOM 形态）
	items := []string{"#whoop手环\n1.2万浏览", "#whoop健康手环\n8万浏览"}
	assert.Equal(t, 0, matchTopicItemIndex(items, "whoop手环"))
}

func TestMatchTopicItemIndex_PrefersExactNotPrefix(t *testing.T) {
	// 输入 whoop 时，即便 whoop手环 排在前面也不能选它，必须命中精确的 whoop
	items := []string{"whoop手环\n1万浏览", "whoop\n2万浏览"}
	assert.Equal(t, 1, matchTopicItemIndex(items, "whoop"))
}

func TestMatchTopicItemIndex_NoMatchReturnsMinusOne(t *testing.T) {
	// 下拉全是无关的推荐/历史话题 → 不匹配 → -1（退化为纯文本，绝不乱选）
	items := []string{"Globall\n9万浏览", "openclaw\n8万浏览", "testsupertopic\n7万浏览"}
	assert.Equal(t, -1, matchTopicItemIndex(items, "whoop"))
}

func TestMatchTopicItemIndex_CampaignTopicWithCountSuffix(t *testing.T) {
	// 活动话题（tag 带前导 #、下拉项名称与浏览量同一行）必须仍能命中，不能回退
	items := []string{"小红书市集66周年庆 1.2亿浏览"}
	assert.Equal(t, 0, matchTopicItemIndex(items, "#小红书市集66周年庆"))
}

func TestMatchTopicItemIndex_CaseInsensitive(t *testing.T) {
	items := []string{"HRV\n5万讨论"}
	assert.Equal(t, 0, matchTopicItemIndex(items, "hrv"))
}

func TestMatchTopicItemIndex_LeadingHashInItem(t *testing.T) {
	items := []string{"#睡眠监测\n3万浏览"}
	assert.Equal(t, 0, matchTopicItemIndex(items, "睡眠监测"))
}

func TestMatchTopicItemIndex_EmptyTagReturnsMinusOne(t *testing.T) {
	items := []string{"whoop\n2万浏览"}
	assert.Equal(t, -1, matchTopicItemIndex(items, ""))
}

func TestMatchTopicItemIndex_NameEndingInCountIsNotStripped(t *testing.T) {
	// 名称本身以「数字+万」结尾、与浏览量分行时，不能把名字尾部当计数剥掉
	// 而误配到更短的 tag（这正是要消灭的误选类 bug）。
	items := []string{"运动1万\n5万浏览"}
	assert.Equal(t, -1, matchTopicItemIndex(items, "运动"))
}

func TestMatchTopicItemIndex_BareCountLikeNameMatches(t *testing.T) {
	// 话题名字面就是「9万」时应能精确命中，不能被计数剥离逻辑吃成空串。
	items := []string{"9万\n1.2万浏览"}
	assert.Equal(t, 0, matchTopicItemIndex(items, "9万"))
}
