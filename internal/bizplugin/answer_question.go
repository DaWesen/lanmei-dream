package bizplugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/DaWesen/lanmei-dream/internal/database"
	pluginpkg "github.com/DaWesen/lanmei-dream/internal/plugin"
	"github.com/fsnotify/fsnotify"
	"github.com/zrurf/conduit"
	"go.uber.org/zap"
)

// ============================================================
// GuessNumberPlugin 编程答题插件
// ============================================================

// GuessNumberPlugin 保留历史类型名、插件 ID 和配置键，当前实现新手编程答题游戏：
//   - /答题：从全部支持语言中开始五题混合答题
//   - /答题 java：选择一种语言，可用空格同时选择多种语言
//   - A / B / C / D：在当前题目中抢答
//
// 每轮固定五道四选一题，每题限时一分钟；第一个答对的人积一分并进入下一题，
// 答错会 @ 玩家，同一玩家在同一道题中最多作答两次。任意题超时会立即结束整轮，
// 完成五题后公布本轮排行榜。轮次状态按群聊或私聊隔离，结算后直接删除，积分不跨轮。
//
// 行为树：
//
//	subtree.guess_number → Selector(
//	  Sequence(isQuizCommand,             Action("pipeline.plugin.guess_number.main")),
//	  Sequence(pass.isActiveQuizAnswer, Action("pipeline.plugin.guess_number.main")),
//	)
//
// 管线：
//
//	pipeline.plugin.guess_number.main → [quizPass]
type GuessNumberPlugin struct {
	kv      *database.PluginKVStore
	quizDir string
	logger  *zap.Logger
	bank    atomic.Pointer[quizBank]
	pass    *quizPass
	watcher *quizBankWatcher
}

// NewGuessNumberPlugin 创建编程答题插件。quizDir 为题库根目录（语言子目录）。
func NewGuessNumberPlugin(quizDir string) *GuessNumberPlugin {
	return &GuessNumberPlugin{quizDir: quizDir}
}

// Info 返回编程答题插件元信息。
func (p *GuessNumberPlugin) Info() pluginpkg.PluginInfo {
	return pluginpkg.PluginInfo{
		ID:          "guess_number",
		Name:        "编程答题",
		Description: "多语言新手选择题抢答（每轮五题、每题两次机会，题库可由 quizdata 目录扩展）",
		Version:     "2.2.0",
		Commands: []pluginpkg.CommandDef{
			{Name: "答题", Description: "开始编程选择题，可指定语言与难度，例如：/答题 go python 困难", Order: 133},
		},
		SubtreeID: pluginpkg.SubtreeID("guess_number"),
	}
}

// OnInit 加载题库并注册编程答题 Pass、Pipeline 和 Subtree。
func (p *GuessNumberPlugin) OnInit(ctx *pluginpkg.PluginContext) error {
	p.kv = ctx.KV
	p.logger = ctx.Logger

	bank, err := loadQuizBank(p.quizDir)
	if err != nil {
		// 题库加载失败不阻断插件注册：以空题库降级，避免 quizdata 未挂载时整机崩溃。
		if p.logger != nil {
			p.logger.Warn("quiz: 题库加载失败，编程答题暂时不可用", zap.Error(err))
		}
		bank = emptyQuizBank()
	} else if p.logger != nil {
		p.logger.Info("quiz: 题库就绪", zap.Int("languages", len(bank.languages)))
	}
	p.bank.Store(bank)
	p.pass = newQuizPass(p.kv, &p.bank)

	passID := pluginpkg.PassID("guess_number", "main")
	if err := ctx.Engine.RegisterPass(passID, p.pass); err != nil {
		return fmt.Errorf("register guess_number pass: %w", err)
	}
	ctx.Registry.TrackPass("guess_number", passID)

	pipelineID := pluginpkg.PipelineID("guess_number", "main")
	if err := ctx.Engine.RegisterPipeline(conduit.NewPipelineFromIDs(pipelineID, passID)); err != nil {
		return fmt.Errorf("register guess_number pipeline: %w", err)
	}
	ctx.Registry.TrackPipeline("guess_number", pipelineID)

	subtree := conduit.NewSelector(
		conduit.NewSequence(
			conduit.NewCondition(isQuizCommand),
			conduit.NewAction(pipelineID),
		),
		conduit.NewSequence(
			conduit.NewCondition(p.pass.isActiveQuizAnswer),
			conduit.NewAction(pipelineID),
		),
	)
	if err := ctx.Engine.RegisterSubtree(pluginpkg.SubtreeID("guess_number"), subtree); err != nil {
		return fmt.Errorf("register guess_number subtree: %w", err)
	}
	return nil
}

// OnStart 启动题库目录监听，支持热更新（文件变更会整体替换题库快照）。
func (p *GuessNumberPlugin) OnStart(_ *pluginpkg.PluginContext) error {
	if p.quizDir == "" {
		return nil
	}
	watcher, err := startQuizBankWatcher(p.quizDir, p.reloadBank)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("quiz: 启动题库热更新监听失败", zap.Error(err))
		}
		return nil
	}
	p.watcher = watcher
	return nil
}

// OnStop 停止题库目录监听、所有题目计时器与异步消息通道。
func (p *GuessNumberPlugin) OnStop(_ *pluginpkg.PluginContext) error {
	if p.watcher != nil {
		p.watcher.close()
		p.watcher = nil
	}
	if p.pass != nil {
		p.pass.stopAll()
	}
	return nil
}

// reloadBank 重新加载题库并原子替换；失败时保留旧题库，保证正在进行的轮次不受影响。
func (p *GuessNumberPlugin) reloadBank() {
	bank, err := loadQuizBank(p.quizDir)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("quiz: 题库热重载失败，继续使用旧题库", zap.Error(err))
		}
		return
	}
	p.bank.Store(bank)
	if p.logger != nil {
		p.logger.Info("quiz: 题库已热重载", zap.Int("languages", len(bank.languages)))
	}
}

// ============================================================
// 题库（数据模型）
// ============================================================
//
// 题目不再硬编码于 Go 源码，而是存放在 quizdata/ 目录下，按「语言目录 →
// *.json」组织（见下方 loadQuizBank）。本节定义数据模型、难度枚举与语言元数据
// 单点表，供加载器、选题逻辑与格式化共用。

// quizLanguage 是题库使用的编程语言标识，即 quizdata/ 下的语言目录名。
type quizLanguage string

// 内置语言常量（历史兼容）。语言目录由目录扫描自动发现，这些常量仅用于
// 元数据登记与测试；新增语言无需在此登记即可被扫描加载。
const (
	quizLanguageJava   quizLanguage = "java"
	quizLanguageGo     quizLanguage = "go"
	quizLanguagePython quizLanguage = "python"
	quizLanguageC      quizLanguage = "c"
	quizLanguageCPP    quizLanguage = "cpp"
)

// quizLanguageMeta 描述一门语言的展示名、别名与展示顺序。
type quizLanguageMeta struct {
	Name    string   // 展示名
	Aliases []string // 玩家可输入的别名词（如 golang / py / c++）
	Order   int      // 展示顺序，小的在前
}

// quizLanguageMetadata 是内置语言的单点元数据来源：展示名、别名解析、
// 帮助文案与语言排序都由此派生，避免在多处硬编码 switch。
//
// 新增语言**无需**在此登记即可被扫描加载（展示名自动标题化、别名等于目录名）；
// 如需更友好的展示名 / 别名，在这里追加一条即可。
var quizLanguageMetadata = map[quizLanguage]quizLanguageMeta{
	quizLanguageJava:   {Name: "Java", Aliases: []string{"java"}, Order: 1},
	quizLanguageGo:     {Name: "Go", Aliases: []string{"go", "golang"}, Order: 2},
	quizLanguagePython: {Name: "Python", Aliases: []string{"python", "py"}, Order: 3},
	quizLanguageC:      {Name: "C", Aliases: []string{"c"}, Order: 4},
	quizLanguageCPP:    {Name: "C++", Aliases: []string{"c++", "c＋＋", "cpp"}, Order: 5},
}

// quizLanguageName 返回语言的展示名；未登记的未知语言回退为标题化目录名。
func quizLanguageName(lang quizLanguage) string {
	if meta, ok := quizLanguageMetadata[lang]; ok {
		return meta.Name
	}
	return titleLanguage(string(lang))
}

// quizLanguageAliases 返回语言的别名词表；未登记的未知语言仅以目录名为别名。
func quizLanguageAliases(lang quizLanguage) []string {
	if meta, ok := quizLanguageMetadata[lang]; ok {
		return meta.Aliases
	}
	return []string{string(lang)}
}

// quizLanguageOrder 返回语言展示顺序；未登记的未知语言排在末尾（按名称稳定排序）。
func quizLanguageOrder(lang quizLanguage) int {
	if meta, ok := quizLanguageMetadata[lang]; ok {
		return meta.Order
	}
	return 1000
}

// titleLanguage 将目录名（如 "rust" / "c++"）转为适合展示的标题形式。
func titleLanguage(id string) string {
	if id == "" {
		return id
	}
	r := []rune(id)
	for i, c := range r {
		if c >= 'a' && c <= 'z' {
			r[i] = c - ('a' - 'A')
			break
		}
	}
	return string(r)
}

// String 返回适合展示给玩家的语言名称。
func (language quizLanguage) String() string {
	return quizLanguageName(language)
}

// quizDifficulty 是题目难度。
type quizDifficulty string

// 难度枚举。JSON 字段值使用小写英文（与中文名解耦，便于序列化），
// 展示时经 String() 转为中文。
const (
	quizDifficultyEasy   quizDifficulty = "easy"
	quizDifficultyMedium quizDifficulty = "medium"
	quizDifficultyHard   quizDifficulty = "hard"
)

// String 返回难度中文名。
func (d quizDifficulty) String() string {
	switch d {
	case quizDifficultyEasy:
		return "简单"
	case quizDifficultyMedium:
		return "中等"
	case quizDifficultyHard:
		return "困难"
	default:
		return ""
	}
}

// valid 判断难度值是否合法。
func (d quizDifficulty) valid() bool {
	switch d {
	case quizDifficultyEasy, quizDifficultyMedium, quizDifficultyHard:
		return true
	default:
		return false
	}
}

// quizQuestion 是一道四选一的编程题，AnswerIndex 使用 0～3 依次对应 A～D。
type quizQuestion struct {
	ID          string         `json:"id"`
	Language    quizLanguage   `json:"language"`
	Prompt      string         `json:"prompt"`
	Options     []string       `json:"options"`
	AnswerIndex int            `json:"answer_index"`
	Explanation string         `json:"explanation"`
	Difficulty  quizDifficulty `json:"difficulty"`
}

// validQuizQuestion 校验单题是否可用于出题与恢复回合。
// 加载题库与恢复回合时共用同一套不变量：必填字段齐全、四选一、答案下标合法、
// 选项无重复、难度合法。
func validQuizQuestion(question quizQuestion) bool {
	if strings.TrimSpace(question.ID) == "" ||
		strings.TrimSpace(question.Prompt) == "" ||
		strings.TrimSpace(question.Explanation) == "" {
		return false
	}
	if len(question.Options) != 4 {
		return false
	}
	if question.AnswerIndex < 0 || question.AnswerIndex > 3 {
		return false
	}
	seen := make(map[string]struct{}, 4)
	for _, option := range question.Options {
		if strings.TrimSpace(option) == "" {
			return false
		}
		if _, dup := seen[option]; dup {
			return false
		}
		seen[option] = struct{}{}
	}
	return question.Difficulty.valid()
}

// ============================================================
// 题库加载与扩展
// ============================================================
//
// 题库采用「目录即语言」的组织方式，扫描过程中自动发现语言与题目，新增语言
// 无需修改任何 Go 代码：
//
//	quizdata/
//	  java/questions.json      # 目录名即语言标识 quizLanguage
//	  go/questions.json
//	  rust/questions.json      # 新增语言：建目录 + 放 JSON 即可
//
// 每个 *.json 文件是一个题目数组，可拆分为多个文件（如 easy.json / hard.json），
// 按难度归类的文件也能被自动合并。单题结构见 quizQuestion。

// quizBank 是一份线程安全的题目快照（加载后不可变，热重载时整体替换）。
type quizBank struct {
	languages []quizLanguage
	questions map[quizLanguage][]quizQuestion
	aliases   map[string]quizLanguage // 归一化别名 → 语言
}

func emptyQuizBank() *quizBank {
	return &quizBank{
		questions: make(map[quizLanguage][]quizQuestion),
		aliases:   make(map[string]quizLanguage),
	}
}

// loadQuizBank 递归扫描 dir 下每个语言子目录里的 *.json 并合并校验。
// 任一份文件解析/校验失败都会整体失败，避免“半截题库”被悄悄启用。
func loadQuizBank(dir string) (*quizBank, error) {
	bank := emptyQuizBank()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("quiz: 读取题库目录 %s 失败: %w", dir, err)
	}

	seenID := make(map[string]quizLanguage)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		language := quizLanguage(entry.Name())
		questions, err := loadLanguageQuestions(language, filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if len(questions) == 0 {
			continue
		}

		bank.languages = append(bank.languages, language)
		bank.questions[language] = questions
		for _, alias := range quizLanguageAliases(language) {
			bank.aliases[normalizeAlias(alias)] = language
		}
		for _, question := range questions {
			if prev, dup := seenID[question.ID]; dup {
				return nil, fmt.Errorf("quiz: 题目 ID %q 在语言 %q 与 %q 之间重复", question.ID, prev, language)
			}
			seenID[question.ID] = language
		}
	}

	sort.Slice(bank.languages, func(i, j int) bool {
		return quizLanguageLess(bank.languages[i], bank.languages[j])
	})
	return bank, nil
}

// loadLanguageQuestions 读取指定语言目录下所有 *.json 文件并合并。
func loadLanguageQuestions(language quizLanguage, dir string) ([]quizQuestion, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("quiz: 读取题库目录 %s 失败: %w", dir, err)
	}

	var all []quizQuestion
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("quiz: 读取题库文件 %s 失败: %w", path, err)
		}

		var questions []quizQuestion
		if err := json.Unmarshal(raw, &questions); err != nil {
			return nil, fmt.Errorf("quiz: 解析题库文件 %s 失败: %w", path, err)
		}

		for i := range questions {
			question := &questions[i]
			question.Language = language
			if question.Difficulty == "" {
				question.Difficulty = quizDifficultyEasy
			}
			if !validQuizQuestion(*question) {
				return nil, fmt.Errorf("quiz: 题库文件 %s 第 %d 题校验失败", path, i+1)
			}
		}
		all = append(all, questions...)
	}
	return all, nil
}

func quizLanguageLess(a, b quizLanguage) bool {
	ao, bo := quizLanguageOrder(a), quizLanguageOrder(b)
	if ao != bo {
		return ao < bo
	}
	return quizLanguageName(a) < quizLanguageName(b)
}

// ============================================================
// 输入解析（语言 + 难度）
// ============================================================

func normalizeAlias(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// isAllKeyword 判断字段是否为“全部语言”关键字。
func isAllKeyword(raw string) bool {
	switch strings.ToLower(raw) {
	case "all", "全部", "混合":
		return true
	default:
		return false
	}
}

// parseDifficultyToken 解析难度关键字（中英文），不支持时返回 false。
func parseDifficultyToken(raw string) (quizDifficulty, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "简单", "easy":
		return quizDifficultyEasy, true
	case "中等", "medium":
		return quizDifficultyMedium, true
	case "困难", "hard":
		return quizDifficultyHard, true
	default:
		return "", false
	}
}

// resolveLanguage 将玩家输入（别名或目录名）解析为该题库已加载的语言。
func (bank *quizBank) resolveLanguage(raw string) (quizLanguage, bool) {
	language, ok := bank.aliases[normalizeAlias(raw)]
	return language, ok
}

// parseQuizCommand 解析 /答题 后的参数：返回语言列表与可选的难度筛选。
// 空参数表示全部语言、不限难度。
func (bank *quizBank) parseQuizCommand(raw string) ([]quizLanguage, quizDifficulty, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return append([]quizLanguage(nil), bank.languages...), "", nil
	}

	normalized := strings.NewReplacer(",", " ", "，", " ", "、", " ", ";", " ", "；", " ").Replace(raw)
	fields := strings.Fields(normalized)

	var languages []quizLanguage
	var difficulty quizDifficulty
	var allRequested bool
	seen := make(map[quizLanguage]bool, len(fields))

	for _, field := range fields {
		if d, ok := parseDifficultyToken(field); ok {
			if difficulty != "" && difficulty != d {
				return nil, "", fmt.Errorf("一轮只能选择一种难度")
			}
			difficulty = d
			continue
		}
		if isAllKeyword(field) {
			allRequested = true
			continue
		}
		language, ok := bank.resolveLanguage(field)
		if !ok {
			return nil, "", fmt.Errorf("暂不支持语言 %q", field)
		}
		if !seen[language] {
			seen[language] = true
			languages = append(languages, language)
		}
	}

	if allRequested {
		if len(languages) > 0 {
			return nil, "", fmt.Errorf("“全部”或“混合”不能与其他语言同时使用")
		}
		languages = append([]quizLanguage(nil), bank.languages...)
	}
	if len(languages) == 0 {
		if len(bank.languages) == 0 {
			return nil, "", fmt.Errorf("题库为空，请联系管理员补充题目")
		}
		// 仅给出难度（如“困难”）或仅分隔符时，默认覆盖全部语言。
		languages = append([]quizLanguage(nil), bank.languages...)
	}
	return languages, difficulty, nil
}

// supportHint 返回错参数时的帮助提示，语言列表由当前题库动态生成。
func (bank *quizBank) supportHint() string {
	if len(bank.languages) == 0 {
		return "题库为空，请联系管理员补充题目"
	}
	return "支持 " + formatQuizLanguages(bank.languages) +
		"；例如：/答题 go python。不写语言时为全部混合，可追加 简单/中等/困难 按难度筛选。"
}

// ============================================================
// 题目抽取
// ============================================================

// errNoDifficultyQuestions 表示所选语言在指定难度下没有任何题目，
// 供调用方转换为友好的用户提示。
var errNoDifficultyQuestions = errors.New("该难度没有题目")

// selectQuestions 从所选语言中均衡抽取题目，按难度筛选（空难度表示不限）。
// 同轮题目不重复，抽取结果随机洗牌。
func (bank *quizBank) selectQuestions(languages []quizLanguage, count int, difficulty quizDifficulty) ([]quizQuestion, error) {
	if len(languages) == 0 || count <= 0 {
		return nil, fmt.Errorf("题目抽取参数无效")
	}

	order := append([]quizLanguage(nil), languages...)
	shuffleQuizLanguages(order)

	// 指定了难度但所有语言都没有该难度的题目时，返回专用哨兵错误。
	if difficulty != "" {
		total := 0
		for _, language := range order {
			total += len(bank.filterQuestions(language, difficulty))
		}
		if total == 0 {
			return nil, errNoDifficultyQuestions
		}
	}
	pools := make(map[quizLanguage][]quizQuestion, len(order))
	positions := make(map[quizLanguage]int, len(order))
	for _, language := range order {
		questions := bank.filterQuestions(language, difficulty)
		if len(questions) == 0 {
			if difficulty == "" {
				return nil, fmt.Errorf("%s 题库为空", quizLanguageName(language))
			}
			return nil, fmt.Errorf("%s 题库中没有「%s」题目", quizLanguageName(language), difficulty)
		}
		shuffleQuizQuestions(questions)
		pools[language] = questions
	}

	selected := make([]quizQuestion, 0, count)
	for len(selected) < count {
		added := false
		for _, language := range order {
			position := positions[language]
			if position >= len(pools[language]) {
				continue
			}
			selected = append(selected, pools[language][position])
			positions[language] = position + 1
			added = true
			if len(selected) == count {
				break
			}
		}
		if !added {
			return nil, fmt.Errorf("所选难度题库不足 %d 题", count)
		}
	}
	shuffleQuizQuestions(selected)
	return selected, nil
}

// filterQuestions 返回指定语言、指定难度下的题目副本（空难度表示全部）。
func (bank *quizBank) filterQuestions(language quizLanguage, difficulty quizDifficulty) []quizQuestion {
	questions := bank.questions[language]
	if difficulty == "" {
		return append([]quizQuestion(nil), questions...)
	}
	filtered := make([]quizQuestion, 0, len(questions))
	for _, question := range questions {
		if question.Difficulty == difficulty {
			filtered = append(filtered, question)
		}
	}
	return filtered
}

func shuffleQuizLanguages(values []quizLanguage) {
	for i := len(values) - 1; i > 0; i-- {
		j := rand.IntN(i + 1)
		values[i], values[j] = values[j], values[i]
	}
}

func shuffleQuizQuestions(values []quizQuestion) {
	for i := len(values) - 1; i > 0; i-- {
		j := rand.IntN(i + 1)
		values[i], values[j] = values[j], values[i]
	}
}

// ============================================================
// 热更新监听
// ============================================================

// quizBankWatcher 监听题库目录的文件变化，防抖后触发重载回调。
type quizBankWatcher struct {
	watcher  *fsnotify.Watcher
	dir      string
	onReload func()
	debounce time.Duration
	done     chan struct{}
	once     sync.Once
}

// startQuizBankWatcher 启动题库目录监听。目录或其子目录不可访问时返回错误。
func startQuizBankWatcher(dir string, onReload func()) (*quizBankWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("quiz: 创建文件监听失败: %w", err)
	}
	if err := addQuizBankWatches(watcher, dir); err != nil {
		watcher.Close()
		return nil, fmt.Errorf("quiz: 添加题库目录监听失败: %w", err)
	}
	qw := &quizBankWatcher{
		watcher:  watcher,
		dir:      dir,
		onReload: onReload,
		debounce: 300 * time.Millisecond,
		done:     make(chan struct{}),
	}
	go qw.run()
	return qw, nil
}

// addQuizBankWatches 监听题库根目录及其所有语言子目录。
func addQuizBankWatches(watcher *fsnotify.Watcher, dir string) error {
	if err := watcher.Add(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if err := watcher.Add(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func (qw *quizBankWatcher) run() {
	var timer *time.Timer
	schedule := func() {
		if timer != nil {
			timer.Stop()
		}
		timer = time.AfterFunc(qw.debounce, func() {
			// 重载前重新同步子目录监听，覆盖“新增语言目录”的场景。
			_ = addQuizBankWatches(qw.watcher, qw.dir)
			qw.onReload()
		})
	}
	for {
		select {
		case _, ok := <-qw.watcher.Events:
			if !ok {
				return
			}
			schedule()
		case _, ok := <-qw.watcher.Errors:
			if !ok {
				return
			}
		case <-qw.done:
			return
		}
	}
}

// close 停止监听并释放资源，幂等。
func (qw *quizBankWatcher) close() {
	qw.once.Do(func() {
		close(qw.done)
		qw.watcher.Close()
	})
}

// ============================================================
// 条件判断与输入解析
// ============================================================

// isQuizCommand 判断消息是否为答题开局命令。
func isQuizCommand(ctx *conduit.MessageContext) bool {
	msg := strings.TrimSpace(ctx.RawMsg)
	return msg == "/答题" || strings.HasPrefix(msg, "/答题 ")
}

// isQuizChoiceMessage 判断消息是否是一个可识别的选项。
func isQuizChoiceMessage(ctx *conduit.MessageContext) bool {
	_, ok := parseQuizChoice(ctx.RawMsg)
	return ok
}

// parseQuizChoice 解析 A～D，兼容小写、全角字母和“答案 A”写法。
func parseQuizChoice(raw string) (int, bool) {
	choice := strings.ToUpper(strings.TrimSpace(raw))
	choice = strings.TrimSpace(strings.TrimPrefix(choice, "答案"))
	switch choice {
	case "A", "Ａ":
		return 0, true
	case "B", "Ｂ":
		return 1, true
	case "C", "Ｃ":
		return 2, true
	case "D", "Ｄ":
		return 3, true
	default:
		return 0, false
	}
}

// ============================================================
// 轮次状态
// ============================================================

const (
	// quizPluginID 沿用历史插件命名空间，避免破坏已有启用配置。
	quizPluginID = "guess_number"
	// quizQuestionCount 每轮固定题数。
	quizQuestionCount = 5
	// quizQuestionTimeout 每道题的固定作答时间。
	quizQuestionTimeout = time.Minute
	// quizAttemptsPerQuestion 每位玩家在同一道题中的最多作答次数。
	quizAttemptsPerQuestion = 2
	// quizStreamBuffer 同时容纳首题与极端情况下紧随其后的超时通知。
	quizStreamBuffer = 2
	// quizBackgroundTimeout 限制后台计时器访问 KV 的最长时间。
	quizBackgroundTimeout = 5 * time.Second
)

// quizKVStore 是答题状态所需的受限 KV 最小接口。
type quizKVStore interface {
	Get(context.Context, string, string) (string, error)
	Set(context.Context, string, string, string) error
	Delete(context.Context, string, string) error
}

// quizGame 保存一轮编程答题的持久化状态。
type quizGame struct {
	Languages        []quizLanguage    `json:"languages"`
	Questions        []quizQuestion    `json:"questions"`
	Current          int               `json:"current"`
	Scores           map[string]int    `json:"scores"`
	Players          map[string]string `json:"players"`
	AnswerAttempts   map[string]int    `json:"answer_attempts"`
	Creator          string            `json:"creator"`
	CreatedAt        int64             `json:"created_at"`
	QuestionDeadline int64             `json:"question_deadline"`
}

// quizRuntime 保存无法持久化的单进程计时器与主动消息通道。
type quizRuntime struct {
	stream     chan string
	timer      *time.Timer
	deadline   int64
	generation uint64
}

// quizKey 生成轮次 KV 键：群聊按群 ID 隔离，私聊按用户 ID 隔离。
func quizKey(groupID, userID string) string {
	if groupID != "" {
		return "state:group:" + groupID
	}
	return "state:dm:" + userID
}

// ============================================================
// 题目展示
// ============================================================

func quizChoiceLetter(index int) string {
	if index < 0 || index > 3 {
		return "?"
	}
	return string(rune('A' + index))
}

func formatQuizLanguages(languages []quizLanguage) string {
	names := make([]string, 0, len(languages))
	for _, language := range languages {
		names = append(names, language.String())
	}
	return strings.Join(names, "、")
}

func formatQuizQuestion(game *quizGame, timeout time.Duration) string {
	question := game.Questions[game.Current]
	seconds := int((timeout + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	label := question.Language.String()
	if difficulty := question.Difficulty.String(); difficulty != "" {
		label += " · " + difficulty
	}
	return fmt.Sprintf(
		"📘 第 %d/%d 题 [%s]\n%s\n\nA. %s\nB. %s\nC. %s\nD. %s\n\n请直接发送 A/B/C/D 抢答（%d 秒内，每人本题最多作答 %d 次）。",
		game.Current+1,
		len(game.Questions),
		label,
		question.Prompt,
		question.Options[0],
		question.Options[1],
		question.Options[2],
		question.Options[3],
		seconds,
		quizAttemptsPerQuestion,
	)
}

func formatQuizRoundStart(game *quizGame, timeout time.Duration) string {
	return fmt.Sprintf(
		"🎓 编程答题开始！\n本轮语言：%s\n规则：共 %d 题，答对 +1 分；每题限时一分钟，超时整轮结束；每人每题最多作答 %d 次。\n\n%s",
		formatQuizLanguages(game.Languages),
		len(game.Questions),
		quizAttemptsPerQuestion,
		formatQuizQuestion(game, timeout),
	)
}

func formatQuizAnswer(question quizQuestion) string {
	return fmt.Sprintf("答案：%s. %s\n解析：%s",
		quizChoiceLetter(question.AnswerIndex),
		question.Options[question.AnswerIndex],
		question.Explanation,
	)
}

type quizRankingEntry struct {
	UserID string
	Name   string
	Score  int
}

func formatQuizLeaderboard(game *quizGame) string {
	entries := make([]quizRankingEntry, 0, len(game.Players))
	for userID, name := range game.Players {
		entries = append(entries, quizRankingEntry{UserID: userID, Name: name, Score: game.Scores[userID]})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Score != entries[j].Score {
			return entries[i].Score > entries[j].Score
		}
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].UserID < entries[j].UserID
	})

	var builder strings.Builder
	builder.WriteString("🏆 本轮积分排行榜")
	for i, entry := range entries {
		builder.WriteString(fmt.Sprintf("\n%d. %s — %d 分", i+1, entry.Name, entry.Score))
	}
	return builder.String()
}

// ============================================================
// Pass 实现
// ============================================================

// quizPass 按命令处理开局与抢答，并串行保护轮次状态和计时器。
type quizPass struct {
	kv               quizKVStore
	bank             *atomic.Pointer[quizBank]
	mu               sync.Mutex
	runtimes         map[string]*quizRuntime
	generationSerial uint64
	questionTimeout  time.Duration
	now              func() time.Time
}

func newQuizPass(kv quizKVStore, bank *atomic.Pointer[quizBank]) *quizPass {
	return &quizPass{
		kv:              kv,
		bank:            bank,
		runtimes:        make(map[string]*quizRuntime),
		questionTimeout: quizQuestionTimeout,
		now:             time.Now,
	}
}

func (pass *quizPass) Execute(ctx *conduit.MessageContext) error {
	if pass.kv == nil {
		pass.reply(ctx, "编程答题功能暂时不可用，请稍后再试~")
		return nil
	}

	msg := strings.TrimSpace(ctx.RawMsg)
	switch {
	case msg == "/答题":
		return pass.start(ctx, "")
	case strings.HasPrefix(msg, "/答题 "):
		argument := strings.TrimSpace(strings.TrimPrefix(msg, "/答题"))
		return pass.start(ctx, argument)
	default:
		choice, ok := parseQuizChoice(msg)
		if !ok {
			return nil
		}
		return pass.answer(ctx, choice)
	}
}

func (pass *quizPass) reply(ctx *conduit.MessageContext, content string) {
	conduit.AppendOutput(ctx, &conduit.Message{
		UserID:  ctx.UserID,
		GroupID: ctx.GroupID,
		IsGroup: ctx.IsGroup,
		Content: content,
	})
}

// replyUser 在 QQ 群聊中 @ 作答用户；其他平台或私聊降级为带昵称的文本。
// at 段统一使用 OneBot 12 的 user_id，网关发送 OB11 时会转换为 qq 字段。
func (pass *quizPass) replyUser(ctx *conduit.MessageContext, content string) {
	platform, _ := ctx.Extra["platform"].(string)
	if !ctx.IsGroup || ctx.UserID == "" || (platform != "qq" && platform != "napcat") {
		pass.reply(ctx, "@"+quizPlayerName(ctx)+content)
		return
	}
	conduit.Set(ctx, sendSegmentsKey, []map[string]any{
		{"type": "at", "data": map[string]any{"user_id": ctx.UserID}},
		{"type": "text", "data": map[string]any{"text": content}},
	})
}

func quizPlayerName(ctx *conduit.MessageContext) string {
	if nickname, _ := ctx.Extra["nickname"].(string); strings.TrimSpace(nickname) != "" {
		return strings.TrimSpace(nickname)
	}
	if strings.TrimSpace(ctx.UserID) != "" {
		return strings.TrimSpace(ctx.UserID)
	}
	return "匿名玩家"
}

// isActiveQuizAnswer 只在当前会话有持久化轮次时接管裸 A～D，
// 避免普通聊天中的单个字母被插件无条件吞掉。
func (pass *quizPass) isActiveQuizAnswer(ctx *conduit.MessageContext) bool {
	if !isQuizChoiceMessage(ctx) {
		return false
	}
	// 在条件节点尽早记录玩家看到的题目代次。若多人同时抢答，第一位答对后
	// 代次会递增，仍在管线中排队的旧题答案不会误算到下一题。
	pass.mu.Lock()
	runtime := pass.runtimes[quizKey(ctx.GroupID, ctx.UserID)]
	if runtime != nil {
		conduit.Set(ctx, quizObservedGenerationKey, runtime.generation)
	}
	pass.mu.Unlock()
	if runtime != nil {
		return true
	}
	// 服务重启后进程内计时器会丢失，但 KV 中可能仍有待清理的轮次；
	// 这种情况下仍接管一次答案消息，由 Pass 给出“轮次中断”提示并清理状态。
	return pass.hasStoredGame(ctx)
}

func (pass *quizPass) hasStoredGame(ctx *conduit.MessageContext) bool {
	if pass.kv == nil {
		return false
	}
	raw, err := pass.kv.Get(ctx.Ctx, quizPluginID, quizKey(ctx.GroupID, ctx.UserID))
	return err == nil && raw != ""
}

// start 开始一轮；同一群或私聊已有未超时轮次时拒绝重复开局。
func (pass *quizPass) start(ctx *conduit.MessageContext, rawLanguages string) error {
	// 插件命令的自然语言意图路径使用同步 Process 重入，当前不会消费 yield 的
	// 流式通道。此处不创建“题目未发出但已经计时”的隐形轮次，明确引导用户
	// 发送斜杠命令；直接输入 /答题 会由高优先级插件子树正常进入异步路径。
	if commandReentry, _ := ctx.Extra[quizCommandReentryKey].(bool); commandReentry {
		command := "/答题"
		if strings.TrimSpace(rawLanguages) != "" {
			command += " " + strings.TrimSpace(rawLanguages)
		}
		pass.reply(ctx, "请直接发送 `"+command+"` 开始编程答题~")
		return nil
	}

	bank := pass.bank.Load()
	if bank == nil {
		pass.reply(ctx, "编程答题题库未加载，请稍后再试~")
		return nil
	}
	languages, difficulty, err := bank.parseQuizCommand(rawLanguages)
	if err != nil {
		pass.reply(ctx, err.Error()+"。\n"+bank.supportHint())
		return nil
	}

	pass.mu.Lock()
	defer pass.mu.Unlock()

	key := quizKey(ctx.GroupID, ctx.UserID)
	existing, expired, err := pass.load(ctx.Ctx, key)
	if err != nil {
		return fmt.Errorf("guess_number: load quiz: %w", err)
	}
	prefix := ""
	if existing != nil && !expired {
		if _, running := pass.runtimes[key]; running {
			pass.reply(ctx, fmt.Sprintf("本群已经有一轮编程答题在进行啦！当前是第 %d/%d 题，请直接发送 A/B/C/D 抢答。", existing.Current+1, len(existing.Questions)))
			return nil
		}
		if err := pass.kv.Delete(ctx.Ctx, quizPluginID, key); err != nil {
			return fmt.Errorf("guess_number: clear interrupted quiz: %w", err)
		}
		prefix = "上一轮因服务重启中断，积分已清零。\n"
	}
	if existing != nil && expired {
		if err := pass.kv.Delete(ctx.Ctx, quizPluginID, key); err != nil {
			return fmt.Errorf("guess_number: clear expired quiz: %w", err)
		}
		prefix = "上一轮已超时结束，积分已清零。\n"
	}
	pass.finishRuntimeLocked(key, "")

	questions, err := bank.selectQuestions(languages, quizQuestionCount, difficulty)
	if err != nil {
		if errors.Is(err, errNoDifficultyQuestions) {
			pass.reply(ctx, "暂时没有该题型，联系工作室的大佬们添加叭~")
			return nil
		}
		return fmt.Errorf("guess_number: select questions: %w", err)
	}
	now := pass.currentTime()
	game := &quizGame{
		Languages:        languages,
		Questions:        questions,
		Scores:           make(map[string]int),
		Players:          make(map[string]string),
		AnswerAttempts:   make(map[string]int),
		Creator:          ctx.UserID,
		CreatedAt:        now.UnixNano(),
		QuestionDeadline: now.Add(pass.timeout()).UnixNano(),
	}
	if err := pass.save(ctx.Ctx, key, game); err != nil {
		return fmt.Errorf("guess_number: save quiz: %w", err)
	}

	stream := make(chan string, quizStreamBuffer)
	pass.runtimes[key] = &quizRuntime{stream: stream, generation: pass.nextGenerationLocked()}
	conduit.Set(ctx, quizStreamChannelKey, stream)
	stream <- prefix + formatQuizRoundStart(game, pass.timeout())
	pass.armTimeoutLocked(key, game.QuestionDeadline)
	return conduit.ErrPassYielded
}

// answer 提交一个选项。第一个答对者得分并推进题目，每位玩家每题最多作答两次。
func (pass *quizPass) answer(ctx *conduit.MessageContext, choice int) error {
	key := quizKey(ctx.GroupID, ctx.UserID)
	observedGeneration, ok := conduit.Get[uint64](ctx, quizObservedGenerationKey)
	if !ok {
		pass.mu.Lock()
		if runtime := pass.runtimes[key]; runtime != nil {
			observedGeneration = runtime.generation
		}
		pass.mu.Unlock()
	}
	return pass.answerForGeneration(ctx, choice, observedGeneration)
}

// answerForGeneration 只把答案计入玩家开始处理时所看到的题目代次。
func (pass *quizPass) answerForGeneration(ctx *conduit.MessageContext, choice int, observedGeneration uint64) error {
	pass.mu.Lock()
	defer pass.mu.Unlock()

	key := quizKey(ctx.GroupID, ctx.UserID)
	game, expired, err := pass.load(ctx.Ctx, key)
	if err != nil {
		return fmt.Errorf("guess_number: load quiz: %w", err)
	}
	if game == nil {
		pass.finishRuntimeLocked(key, "")
		pass.reply(ctx, "现在没有进行中的编程答题，用 `/答题` 开始一轮吧。")
		return nil
	}
	if expired {
		question := game.Questions[game.Current]
		if err := pass.kv.Delete(ctx.Ctx, quizPluginID, key); err != nil {
			return fmt.Errorf("guess_number: expire quiz: %w", err)
		}
		pass.finishRuntimeLocked(key, "")
		pass.reply(ctx, fmt.Sprintf("⏰ 第 %d 题已超时，%s。\n本轮直接结束，积分已清零。", game.Current+1, formatQuizAnswer(question)))
		return nil
	}
	runtime, running := pass.runtimes[key]
	if !running {
		if err := pass.kv.Delete(ctx.Ctx, quizPluginID, key); err != nil {
			return fmt.Errorf("guess_number: clear interrupted quiz: %w", err)
		}
		pass.reply(ctx, "本轮因服务重启中断，积分已清零。请用 `/答题` 重新开始。")
		return nil
	}
	if runtime.generation != observedGeneration {
		pass.replyUser(ctx, " 上一题已经有人答对啦，请回答最新题目~")
		return nil
	}
	if strings.TrimSpace(ctx.UserID) == "" {
		pass.reply(ctx, "无法识别你的用户 ID，暂时不能参与抢答~")
		return nil
	}

	userID := ctx.UserID
	attempts := game.AnswerAttempts[userID]
	if attempts >= quizAttemptsPerQuestion {
		pass.replyUser(ctx, " 本题的两次作答机会已经用完啦，其他人继续抢答~")
		return nil
	}
	game.Players[userID] = quizPlayerName(ctx)
	game.AnswerAttempts[userID] = attempts + 1
	question := game.Questions[game.Current]
	if choice != question.AnswerIndex {
		if err := pass.save(ctx.Ctx, key, game); err != nil {
			return fmt.Errorf("guess_number: save wrong answer: %w", err)
		}
		remaining := quizAttemptsPerQuestion - game.AnswerAttempts[userID]
		if remaining > 0 {
			pass.replyUser(ctx, fmt.Sprintf(" 答错了，本题还剩 %d 次机会，继续加油~", remaining))
		} else {
			pass.replyUser(ctx, " 答错了，本题的两次机会已经用完，其他人继续抢答~")
		}
		return nil
	}

	game.Scores[userID]++
	currentScore := game.Scores[userID]
	game.Current++
	if game.Current == len(game.Questions) {
		if err := pass.kv.Delete(ctx.Ctx, quizPluginID, key); err != nil {
			return fmt.Errorf("guess_number: finish quiz: %w", err)
		}
		pass.finishRuntimeLocked(key, "")
		pass.replyUser(ctx, fmt.Sprintf(
			" 🎉 答对啦！+1 分，本轮五题全部完成。\n✅ %s\n\n%s\n\n本轮积分现已清零，再用 `/答题` 可以开始新一轮。",
			formatQuizAnswer(question),
			formatQuizLeaderboard(game),
		))
		return nil
	}

	game.AnswerAttempts = make(map[string]int)
	game.QuestionDeadline = pass.currentTime().Add(pass.timeout()).UnixNano()
	if err := pass.save(ctx.Ctx, key, game); err != nil {
		return fmt.Errorf("guess_number: advance quiz: %w", err)
	}
	runtime.generation = pass.nextGenerationLocked()
	pass.armTimeoutLocked(key, game.QuestionDeadline)
	pass.replyUser(ctx, fmt.Sprintf(
		" 🎉 答对啦！+1 分，当前 %d 分。\n✅ %s\n\n%s",
		currentScore,
		formatQuizAnswer(question),
		formatQuizQuestion(game, pass.timeout()),
	))
	return nil
}

// ============================================================
// 持久化与主动超时
// ============================================================

// load 读取当前轮次，并返回当前题是否已到截止时间。
// 历史猜数字状态或损坏状态会被识别并清理。
func (pass *quizPass) load(ctx context.Context, key string) (*quizGame, bool, error) {
	raw, err := pass.kv.Get(ctx, quizPluginID, key)
	if err != nil || raw == "" {
		return nil, false, err
	}
	var game quizGame
	if err := json.Unmarshal([]byte(raw), &game); err != nil {
		if deleteErr := pass.kv.Delete(ctx, quizPluginID, key); deleteErr != nil {
			return nil, false, fmt.Errorf("delete malformed state after decode error %v: %w", err, deleteErr)
		}
		return nil, false, nil
	}
	if !validQuizGame(&game) {
		if err := pass.kv.Delete(ctx, quizPluginID, key); err != nil {
			return nil, false, fmt.Errorf("delete invalid state: %w", err)
		}
		return nil, false, nil
	}
	if game.Scores == nil {
		game.Scores = make(map[string]int)
	}
	if game.Players == nil {
		game.Players = make(map[string]string)
	}
	if game.AnswerAttempts == nil {
		game.AnswerAttempts = make(map[string]int)
	}
	expired := !pass.currentTime().Before(time.Unix(0, game.QuestionDeadline))
	return &game, expired, nil
}

func validQuizGame(game *quizGame) bool {
	if game == nil || len(game.Languages) == 0 || len(game.Questions) != quizQuestionCount {
		return false
	}
	if game.Current < 0 || game.Current >= len(game.Questions) || game.QuestionDeadline <= 0 {
		return false
	}
	for _, question := range game.Questions {
		if !validQuizQuestion(question) {
			return false
		}
	}
	return true
}

// save 保存当前轮次。
func (pass *quizPass) save(ctx context.Context, key string, game *quizGame) error {
	data, err := json.Marshal(game)
	if err != nil {
		return err
	}
	return pass.kv.Set(ctx, quizPluginID, key, string(data))
}

func (pass *quizPass) currentTime() time.Time {
	if pass.now != nil {
		return pass.now()
	}
	return time.Now()
}

func (pass *quizPass) timeout() time.Duration {
	if pass.questionTimeout > 0 {
		return pass.questionTimeout
	}
	return quizQuestionTimeout
}

// nextGenerationLocked 为每一道新题分配进程内唯一代次。调用方必须持有 pass.mu。
func (pass *quizPass) nextGenerationLocked() uint64 {
	pass.generationSerial++
	if pass.generationSerial == 0 {
		pass.generationSerial++
	}
	return pass.generationSerial
}

// armTimeoutLocked 为当前题目启动或重置主动超时计时器。调用方必须持有 pass.mu。
func (pass *quizPass) armTimeoutLocked(key string, deadline int64) {
	runtime := pass.runtimes[key]
	if runtime == nil {
		return
	}
	if runtime.timer != nil {
		runtime.timer.Stop()
	}
	runtime.deadline = deadline
	delay := time.Unix(0, deadline).Sub(pass.currentTime())
	if delay < 0 {
		delay = 0
	}
	runtime.timer = time.AfterFunc(delay, func() {
		pass.expire(key, deadline, runtime)
	})
}

// expire 在一分钟截止时删除整轮状态，并通过开局消息的异步通道主动通知会话。
func (pass *quizPass) expire(key string, deadline int64, expected *quizRuntime) {
	ctx, cancel := context.WithTimeout(context.Background(), quizBackgroundTimeout)
	defer cancel()

	pass.mu.Lock()
	defer pass.mu.Unlock()

	runtime := pass.runtimes[key]
	if runtime == nil || runtime != expected || runtime.deadline != deadline {
		return
	}
	game, expired, err := pass.load(ctx, key)
	if err != nil {
		pass.finishRuntimeLocked(key, "⏰ 本题计时结束，本轮已结束，积分已清零。")
		return
	}
	if game == nil {
		pass.finishRuntimeLocked(key, "")
		return
	}
	if game.QuestionDeadline != deadline {
		return
	}
	if !expired {
		pass.armTimeoutLocked(key, deadline)
		return
	}

	_ = pass.kv.Delete(ctx, quizPluginID, key)
	question := game.Questions[game.Current]
	pass.finishRuntimeLocked(key, fmt.Sprintf(
		"⏰ 第 %d 题已超过一分钟，%s。\n本轮直接结束，积分已清零。",
		game.Current+1,
		formatQuizAnswer(question),
	))
}

// finishRuntimeLocked 停止计时器，可选发送最后一条主动消息，然后关闭通道。
// 调用方必须持有 pass.mu。
func (pass *quizPass) finishRuntimeLocked(key, finalMessage string) {
	runtime := pass.runtimes[key]
	if runtime == nil {
		return
	}
	if runtime.timer != nil {
		runtime.timer.Stop()
	}
	delete(pass.runtimes, key)
	if finalMessage != "" {
		select {
		case runtime.stream <- finalMessage:
		default:
		}
	}
	close(runtime.stream)
}

// stopAll 在插件停止时释放所有进程内计时器和通道。
func (pass *quizPass) stopAll() {
	pass.mu.Lock()
	defer pass.mu.Unlock()
	for key := range pass.runtimes {
		pass.finishRuntimeLocked(key, "")
	}
}

const (
	// quizStreamChannelKey 复用 bot 层流式段落通道，让一分钟超时可以主动发送消息。
	quizStreamChannelKey = "bot.stream.ch"
	// quizObservedGenerationKey 保存答案消息进入行为树时看到的题目代次。
	quizObservedGenerationKey = "quiz.observed_generation"
	// quizCommandReentryKey 标记由自然语言意图触发的插件命令同步重入。
	quizCommandReentryKey = "bot.command.reentry"
)
