package requestarchive

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode"
)

// redactedValue 是所有敏感值统一使用的替换内容。保留 JSON 键、仅替换值，
// 使归档仍能用于排查请求结构问题。
const redactedValue = "[REDACTED]"

// archiveSensitiveKeys 使用归一化后的键名，因此 api_key、api-key、APIKey
// 都会命中相同规则。这些字段即使内容没有匹配下面的文本正则，也必须脱敏。
var archiveSensitiveKeys = map[string]struct{}{
	"authorization": {}, "proxyauthorization": {},
	"apikey": {}, "xapikey": {}, "token": {}, "accesstoken": {}, "refreshtoken": {}, "idtoken": {}, "sessiontoken": {},
	"secret": {}, "clientsecret": {}, "appsecret": {}, "password": {}, "passwd": {}, "pwd": {},
	"cookie": {}, "setcookie": {}, "privatekey": {}, "secretaccesskey": {}, "credential": {}, "credentials": {},
	"signature": {}, "encryptedcontent": {}, "thinking": {}, "reasoningcontent": {},
	"email": {}, "emailaddress": {}, "useremail": {}, "phone": {}, "phonenumber": {}, "mobile": {}, "mobilephone": {}, "telephone": {},
	"idcard": {}, "idnumber": {}, "identitynumber": {}, "nationalid": {}, "ssn": {},
	"address": {}, "streetaddress": {}, "postaladdress": {},
	"username": {}, "userid": {}, "user": {},
	"accountname": {}, "accountusername": {}, "loginname": {}, "accountnumber": {},
	"bankaccount": {}, "bankaccountnumber": {}, "bankcard": {}, "bankcardnumber": {},
	"cardnumber": {}, "creditcard": {}, "creditcardnumber": {}, "debitcard": {}, "debitcardnumber": {},
	"cvv": {}, "cvc": {}, "securitycode": {}, "cardholder": {},
	"b64json": {},

	// 中文字段名。normalizeArchiveKey 会保留中文字符，因此直接列出常见写法。
	"密码": {}, "口令": {}, "密钥": {}, "秘钥": {}, "api密钥": {},
	"令牌": {}, "访问令牌": {}, "刷新令牌": {}, "身份令牌": {}, "授权": {}, "授权码": {}, "凭证": {},
	"私钥": {}, "签名": {}, "思考": {}, "推理内容": {}, "加密内容": {},
	"邮箱": {}, "电子邮箱": {}, "手机号": {}, "手机号码": {}, "电话": {}, "电话号码": {},
	"身份证": {}, "身份证号": {}, "地址": {}, "住址": {}, "收货地址": {}, "详细地址": {},
	"姓名": {}, "用户名": {}, "用户": {}, "用户id": {}, "用户标识": {},
	"账户": {}, "账户名": {}, "账户名称": {}, "登录名": {}, "账号": {}, "账号名": {}, "账号名称": {}, "账户号": {}, "账号号": {},
	"银行账户": {}, "银行账号": {}, "银行卡": {}, "银行卡号": {}, "卡号": {}, "信用卡": {}, "信用卡号": {}, "借记卡": {}, "借记卡号": {},
	"安全码": {}, "持卡人": {},
}

var (
	// 这些正则用于识别普通提示词、工具参数或模型输出文本中嵌入的敏感片段，
	// 它们是上方“按字段名脱敏”规则的补充。
	archiveEmailPattern             = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	archivePhonePattern             = regexp.MustCompile(`(?:\+?\d[\d\s().-]{8,}\d)`)
	archiveChinaIDPattern           = regexp.MustCompile(`\b\d{17}[0-9Xx]\b`)
	archiveBearerPattern            = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+\-/]+=*`)
	archiveLabeledSecretPattern     = regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|password|passwd|pwd|secret)\b(\s*[:=]\s*|\s+)["']?[A-Za-z0-9._~+\-/=]{6,}["']?`)
	archiveEnglishLabeledPIIPattern = regexp.MustCompile(`(?i)\b(account[_ -]?name|login[_ -]?name|account[_ -]?number|bank[_ -]?account(?:[_ -]?number)?|bank[_ -]?card(?:[_ -]?number)?|card[_ -]?number|credit[_ -]?card|debit[_ -]?card|cvv|cvc|security[_ -]?code|card[_ -]?holder)\b(\s*[:=]\s*|\s+)["']?[^\s,;]+`)
	archiveChineseLabeledPattern    = regexp.MustCompile(`(密码|口令|密钥|秘钥|令牌|访问令牌|刷新令牌|身份令牌|授权码|凭证|身份证(?:号)?|手机号|手机号码|电话号码|邮箱|电子邮箱|收货地址|家庭住址|住址|地址|姓名|账户名|账户名称|登录名|账号名|账号名称|账户号|账号号|银行账户|银行账号|银行卡(?:号)?|信用卡(?:号)?|借记卡(?:号)?|卡号|安全码|持卡人)\s*(?:是|为|[:：=])\s*["“”']?[^\s，,。；;]+`)
	archiveKnownKeyPattern          = regexp.MustCompile(`(?i)\b(?:sk|rk|pk)-[A-Za-z0-9_-]{8,}\b|\bAIza[0-9A-Za-z_-]{35}\b|\bGOCSPX-[0-9A-Za-z_-]{16,}\b|\bAKIA[0-9A-Z]{16}\b|\b(?:ghp|gho|ghu|ghs|github_pat|xox[baprs])_[A-Za-z0-9_-]{8,}\b`)
	archiveJWTPattern               = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
)

// redactJSON 将一个 JSON 文档转换为可安全落盘的归档副本。
//
// 有意使用 json.Number，避免大的 Token 数或 ID 经 float64 转换后丢失精度。
// 嵌套字段替换后无法安全保留原始空白和键顺序，因此会重新序列化 JSON。
// 本函数只处理归档副本；调用方继续向上游和客户端传递原始字节。
func redactJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values")
		}
		return nil, err
	}
	return json.Marshal(redactArchiveValue(value, "", 0))
}

// redactArchiveValue 递归遍历对象和数组。key 是当前值所在的字段名，
// 因此名为 password 的字段无论值是字符串、数字、数组还是对象都会被整体脱敏。
func redactArchiveValue(value any, key string, depth int) any {
	if depth > 64 {
		// 恶意或异常深度的 JSON 不应造成无限递归。超过限制后停止继续检查，
		// 按归档兼容性要求原样保留当前子树，不改变它的 JSON 结构。
		return value
	}
	if isArchiveSensitiveKey(key) {
		return redactedValue
	}
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for childKey, childValue := range typed {
			out[childKey] = redactArchiveValue(childValue, childKey, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = redactArchiveValue(item, key, depth+1)
		}
		return out
	case string:
		if isEmbeddedPrivateMedia(key, typed) {
			return redactedValue
		}
		return redactArchiveText(typed)
	default:
		return value
	}
}

// isArchiveSensitiveKey 使用归一化后的 JSON 键名判断是否属于敏感字段。
func isArchiveSensitiveKey(key string) bool {
	_, ok := archiveSensitiveKeys[normalizeArchiveKey(key)]
	return ok
}

// normalizeArchiveKey 忽略 JSON 键名中的大小写和分隔符差异。
func normalizeArchiveKey(key string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, strings.TrimSpace(key))
}

// isEmbeddedPrivateMedia 识别 JSON 中内嵌的媒体。图片、文件、语音或截图
// 可能包含人脸、证件或其他隐私，因此整个值都不落盘。
func isEmbeddedPrivateMedia(key, value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(lower, "data:image/") || strings.HasPrefix(lower, "data:audio/") || strings.HasPrefix(lower, "data:video/") || strings.HasPrefix(lower, "data:application/pdf") {
		return true
	}
	normalized := normalizeArchiveKey(key)
	return (normalized == "data" || normalized == "image" || normalized == "audio" || normalized == "file") && len(value) >= 256 && looksLikeBase64(value)
}

// looksLikeBase64 是轻量级启发式判断。它只用于媒体相关键下的长字符串，
// 不会因为普通文本恰好只含 Base64 合法字符就把文本删除。
func looksLikeBase64(value string) bool {
	for _, r := range value {
		if unicode.IsSpace(r) || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

// redactArchiveText 处理自由文本中的敏感片段，此类数据没有可依赖的 JSON 键名。
// 规则只替换敏感部分，尽量保留其余文本以便排查问题。
func redactArchiveText(value string) string {
	value = archiveBearerPattern.ReplaceAllString(value, "Bearer "+redactedValue)
	value = archiveLabeledSecretPattern.ReplaceAllString(value, "$1$2"+redactedValue)
	value = archiveEnglishLabeledPIIPattern.ReplaceAllString(value, "$1$2"+redactedValue)
	// 中文自由文本通常以“字段名：值”或“字段名是值”的方式携带敏感信息。
	// 保留字段名，替换紧随其后的值，便于理解被脱敏的原因。
	value = archiveChineseLabeledPattern.ReplaceAllString(value, "$1: "+redactedValue)
	value = archiveKnownKeyPattern.ReplaceAllString(value, redactedValue)
	value = archiveJWTPattern.ReplaceAllString(value, redactedValue)
	value = archiveEmailPattern.ReplaceAllString(value, redactedValue)
	value = archiveChinaIDPattern.ReplaceAllString(value, redactedValue)
	value = archivePhonePattern.ReplaceAllStringFunc(value, func(candidate string) string {
		digits := 0
		for _, r := range candidate {
			if unicode.IsDigit(r) {
				digits++
			}
		}
		if digits >= 10 {
			return redactedValue
		}
		return candidate
	})
	return value
}
