package random_beauty

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxMetadataRunes = 256
	maxMetadataTags  = 128
)

// mandatoryExcludedTags 是不可通过配置放宽的安全下限。
var mandatoryExcludedTags = []string{
	"R-18", "R-18G", "NSFW", "裸体", "全裸", "裸胸", "乳首", "乳房",
	"性行为", "色情", "情色", "性器官", "成人用品", "内衣", "泳装", "比基尼",
	"走光", "透视", "湿身", "浴巾", "裸露上身", "スク水", "水着", "下着",
	"魅惑のふともも", "underwear", "lingerie", "swimsuit", "school swimsuit",
	"bikini", "nude", "naked", "explicit", "sexualized", "sex toy", "fetish",
	"ビキニ", "谷間", "お尻", "ハイレグ", "バニーガール", "兔女郎", "肚脐",
	"ロリ", "萝莉", "幼女", "loli", "ショタ", "shota", "ecchi", "lewd", "bondage",
}

// normalizedDeniedTerms 用于本地保守复核；匹配前会去除空白与常见分隔符。
var normalizedDeniedTerms = func() []string {
	terms := make([]string, 0, len(mandatoryExcludedTags)+12)
	for _, term := range mandatoryExcludedTags {
		terms = append(terms, normalizeMetadata(term))
	}
	terms = append(terms,
		"おっぱい", "パンツ", "パンチラ", "透けて", "透け", "ノーブラ",
		"cleavage", "sideboob", "underboob", "cameltoe", "upskirt", "topless",
	)
	return terms
}()

func excludedTags() []string {
	return append([]string(nil), mandatoryExcludedTags...)
}

func metadataSafe(candidate *Candidate) bool {
	if !metadataWithinLimits(candidate) {
		return false
	}
	fields := make([]string, 0, len(candidate.Tags)+2)
	fields = append(fields, candidate.Title, candidate.Author)
	fields = append(fields, candidate.Tags...)
	for _, field := range fields {
		normalized := normalizeMetadata(field)
		for _, denied := range normalizedDeniedTerms {
			if denied != "" && strings.Contains(normalized, denied) {
				return false
			}
		}
	}
	return true
}

func metadataWithinLimits(candidate *Candidate) bool {
	if candidate == nil || len(candidate.Tags) > maxMetadataTags {
		return false
	}
	if utf8.RuneCountInString(candidate.Title) > maxMetadataRunes || utf8.RuneCountInString(candidate.Author) > maxMetadataRunes {
		return false
	}
	for _, tag := range candidate.Tags {
		if utf8.RuneCountInString(tag) > maxMetadataRunes {
			return false
		}
	}
	return true
}

func normalizeMetadata(value string) string {
	return strings.Map(func(r rune) rune {
		// 折叠全角 ASCII，避免 Ｒ－１８、Ｓｗｉｍｓｕｉｔ 等书写绕过。
		if r >= 0xFF01 && r <= 0xFF5E {
			r -= 0xFEE0
		}
		if unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			return -1
		}
		return unicode.ToLower(r)
	}, value)
}
