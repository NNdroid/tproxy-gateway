package main

import (
	"bufio"
	"bytes"
	"fmt"
	"net/textproto"
)

// rewriteHTTPHeader 解析並修改 HTTP 請求頭
func rewriteHTTPHeader(reader *bufio.Reader, rewrites map[string]string) ([]byte, error) {
	// 1. 讀取請求行 (例如 GET / HTTP/1.1)
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}

	var newHeader bytes.Buffer
	newHeader.WriteString(line)

	// 2. 使用 textproto 讀取頭部
	tp := textproto.NewReader(reader)
	mimeHeader, err := tp.ReadMIMEHeader()
	if err != nil {
		return nil, err
	}

	// 3. 執行替換
	for key, newValue := range rewrites {
		mimeHeader.Set(key, newValue)
	}

	// 4. 重新封包頭部
	for key, values := range mimeHeader {
		for _, v := range values {
			newHeader.WriteString(fmt.Sprintf("%s: %s\r\n", key, v))
		}
	}
	newHeader.WriteString("\r\n") // 頭部結束標記

	return newHeader.Bytes(), nil
}
