package voice

import "strings"

// LanguageCode is hMe() (mod_0019): map a settings.language value to an
// STT language code. Returns the resolved code and, when the input fell
// back to English, the original value (fellBackFrom).
var sttLanguages = map[string]bool{
	"en": true, "es": true, "fr": true, "ja": true, "de": true,
	"pt": true, "it": true, "ko": true, "hi": true, "id": true,
	"ru": true, "pl": true, "tr": true, "nl": true, "uk": true,
	"el": true, "cs": true, "da": true, "sv": true, "no": true,
}

var langAliases = map[string]string{
	"english": "en", "spanish": "es", "español": "es", "espanol": "es",
	"french": "fr", "français": "fr", "francais": "fr",
	"japanese": "ja", "日本語": "ja",
	"german": "de", "deutsch": "de",
	"portuguese": "pt", "português": "pt", "portugues": "pt",
	"italian": "it", "italiano": "it",
	"korean": "ko", "한국어": "ko",
	"hindi": "hi", "हिन्दी": "hi", "हिंदी": "hi",
	"indonesian": "id", "bahasa indonesia": "id", "bahasa": "id",
	"russian": "ru", "русский": "ru",
	"polish": "pl", "polski": "pl",
	"turkish": "tr", "türkçe": "tr", "turkce": "tr",
	"dutch": "nl", "nederlands": "nl",
	"ukrainian": "uk", "українська": "uk",
	"greek": "el", "ελληνικά": "el",
	"czech": "cs", "čeština": "cs", "cestina": "cs",
	"danish": "da", "dansk": "da",
	"swedish": "sv", "svenska": "sv",
	"norwegian": "no", "norsk": "no",
}

// LanguageCode resolves a language setting (name or BCP-47-ish code) to
// an STT code. fellBackFrom is non-empty when the input wasn't recognized.
func LanguageCode(setting string) (code string, fellBackFrom string) {
	n := strings.ToLower(strings.TrimSpace(setting))
	if n == "" {
		return "en", ""
	}
	if sttLanguages[n] {
		return n, ""
	}
	if r, ok := langAliases[n]; ok {
		return r, ""
	}
	if base := strings.SplitN(n, "-", 2)[0]; base != "" && sttLanguages[base] {
		return base, ""
	}
	return "en", setting
}
