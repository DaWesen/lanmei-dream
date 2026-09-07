package random_beauty

import "testing"

func TestMetadataFilterRejectsUnsafeTerms(t *testing.T) {
	tests := []Candidate{
		{Tags: []string{"R-18"}},
		{Tags: []string{"スク水"}},
		{Tags: []string{"School Swimsuit"}},
		{Title: "透 け て 見える服"},
		{Tags: []string{"underwear"}},
		{Tags: []string{"乳首"}},
		{Tags: []string{"未成年", "sexualized"}},
		{Tags: []string{"魅惑のふともも"}},
		{Tags: []string{"ビキニ"}},
		{Tags: []string{"谷間"}},
		{Tags: []string{"お尻"}},
		{Tags: []string{"ハイレグ"}},
		{Tags: []string{"バニーガール"}},
		{Tags: []string{"兔女郎"}},
		{Tags: []string{"肚脐"}},
		{Tags: []string{"萝莉"}},
		{Tags: []string{"loli"}},
		{Tags: []string{"ショタ"}},
		{Tags: []string{"ecchi"}},
		{Tags: []string{"lewd"}},
		{Tags: []string{"bondage"}},
		{Tags: []string{"Ｒ－１８"}},
		{Tags: []string{"Ｓｗｉｍｓｕｉｔ"}},
	}

	for _, candidate := range tests {
		if metadataSafe(&candidate) {
			t.Errorf("metadataSafe(%+v) = true", candidate)
		}
	}
}

func TestMetadataFilterAllowsOrdinaryContent(t *testing.T) {
	tests := []Candidate{
		{Title: "雨后的城市", Tags: []string{"风景", "城市"}},
		{Title: "猫", Tags: []string{"动物", "可爱"}},
		{Title: "午餐", Tags: []string{"食物", "料理"}},
		{Title: "同学合影", Tags: []string{"学校", "制服", "友情"}},
		{Title: "旅途", Author: "普通画师", Tags: []string{"人物", "全身"}},
	}

	for _, candidate := range tests {
		if !metadataSafe(&candidate) {
			t.Errorf("metadataSafe(%+v) = false", candidate)
		}
	}
}

func TestMetadataFilterRejectsOversizedMetadata(t *testing.T) {
	long := make([]rune, maxMetadataRunes+1)
	for i := range long {
		long[i] = '画'
	}
	tooManyTags := make([]string, maxMetadataTags+1)
	for i := range tooManyTags {
		tooManyTags[i] = "风景"
	}

	if metadataSafe(&Candidate{Title: string(long)}) {
		t.Fatal("long title should be rejected")
	}
	if metadataSafe(&Candidate{Tags: tooManyTags}) {
		t.Fatal("too many tags should be rejected")
	}
}

func TestExcludedTagsAreAlwaysPresent(t *testing.T) {
	tags := excludedTags()
	if len(tags) < 10 {
		t.Fatalf("excludedTags() returned only %d entries", len(tags))
	}
	tags[0] = "mutated"
	if excludedTags()[0] == "mutated" {
		t.Fatal("excludedTags() exposed mutable backing storage")
	}
}
