package ai

import "errors"

var (
	// ErrEmptyResponse LLM 返回了空内容
	ErrEmptyResponse = errors.New("empty response from LLM")
	// ErrContextTooLong 上下文长度超限
	ErrContextTooLong = errors.New("context too long")
	// ErrEmbeddingFailed 向量生成失败
	ErrEmbeddingFailed = errors.New("embedding generation failed")
	// ErrMemoryNotFound 未找到相关记忆
	ErrMemoryNotFound = errors.New("memory not found")
)
