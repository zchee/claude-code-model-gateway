// Copyright 2026 The claude-code-model-gateway Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package toolschema

import (
	"strings"
	"unicode"
)

// ECMA-262, 16th edition, §22.2.2.9, Tables 69–70. Table 71 properties
// describe strings and are not available with the u flag. Alias spelling is exact.
func validProperty(expr string) bool {
	name, value, pair := strings.Cut(expr, "=")
	if !pair {
		value = name
	}
	if !pair || name == "gc" || name == "General_Category" {
		if unicode.Categories[value] != nil || unicode.CategoryAliases[value] != "" {
			return true
		}
	}
	if pair {
		switch name {
		case "sc", "Script", "scx", "Script_Extensions":
			return validScript(value)
		}
		return false
	}
	switch name {
	case "ASCII", "ASCII_Hex_Digit", "AHex", "Alphabetic", "Alpha", "Any", "Assigned", "Bidi_Control", "Bidi_C", "Bidi_Mirrored", "Bidi_M",
		"Case_Ignorable", "CI", "Cased", "Changes_When_Casefolded", "CWCF", "Changes_When_Casemapped", "CWCM", "Changes_When_Lowercased", "CWL",
		"Changes_When_NFKC_Casefolded", "CWKCF", "Changes_When_Titlecased", "CWT", "Changes_When_Uppercased", "CWU", "Dash", "Default_Ignorable_Code_Point", "DI",
		"Deprecated", "Dep", "Diacritic", "Dia", "Emoji", "Emoji_Component", "EComp", "Emoji_Modifier", "EMod", "Emoji_Modifier_Base", "EBase", "Emoji_Presentation", "EPres",
		"Extended_Pictographic", "ExtPict", "Extender", "Ext", "Grapheme_Base", "Gr_Base", "Grapheme_Extend", "Gr_Ext", "Hex_Digit", "Hex", "IDS_Binary_Operator", "IDSB",
		"IDS_Trinary_Operator", "IDST", "ID_Continue", "IDC", "ID_Start", "IDS", "Ideographic", "Ideo", "Join_Control", "Join_C", "Logical_Order_Exception", "LOE",
		"Lowercase", "Lower", "Math", "Noncharacter_Code_Point", "NChar", "Pattern_Syntax", "Pat_Syn", "Pattern_White_Space", "Pat_WS", "Quotation_Mark", "QMark",
		"Radical", "Regional_Indicator", "RI", "Sentence_Terminal", "STerm", "Soft_Dotted", "SD", "Terminal_Punctuation", "Term", "Unified_Ideograph", "UIdeo",
		"Uppercase", "Upper", "Variation_Selector", "VS", "White_Space", "space", "XID_Continue", "XIDC", "XID_Start", "XIDS":
		return true
	}
	return false
}

// Script values come from Unicode 16.0 PropertyValueAliases.txt, the UCD used
// by the pinned Node. V8 rejects Hrkt/Katakana_Or_Hiragana: its ICU set is empty.
// Use an allowlist so upgrading Go's Unicode tables cannot introduce new scripts.
func validScript(value string) bool {
	switch value {
	case "Adlm", "Adlam", "Aghb", "Caucasian_Albanian", "Ahom", "Arab", "Arabic", "Armi", "Imperial_Aramaic", "Armn", "Armenian", "Avst", "Avestan",
		"Bali", "Balinese", "Bamu", "Bamum", "Bass", "Bassa_Vah", "Batk", "Batak", "Beng", "Bengali", "Bhks", "Bhaiksuki", "Bopo", "Bopomofo", "Brah", "Brahmi",
		"Brai", "Braille", "Bugi", "Buginese", "Buhd", "Buhid", "Cakm", "Chakma", "Cans", "Canadian_Aboriginal", "Cari", "Carian", "Cham", "Cher", "Cherokee",
		"Chrs", "Chorasmian", "Copt", "Coptic", "Qaac", "Cpmn", "Cypro_Minoan", "Cprt", "Cypriot", "Cyrl", "Cyrillic", "Deva", "Devanagari", "Diak", "Dives_Akuru",
		"Dogr", "Dogra", "Dsrt", "Deseret", "Dupl", "Duployan", "Egyp", "Egyptian_Hieroglyphs", "Elba", "Elbasan", "Elym", "Elymaic", "Ethi", "Ethiopic", "Gara", "Garay",
		"Geor", "Georgian", "Glag", "Glagolitic", "Gong", "Gunjala_Gondi", "Gonm", "Masaram_Gondi", "Goth", "Gothic", "Gran", "Grantha", "Grek", "Greek", "Gujr", "Gujarati",
		"Gukh", "Gurung_Khema", "Guru", "Gurmukhi", "Hang", "Hangul", "Hani", "Han", "Hano", "Hanunoo", "Hatr", "Hatran", "Hebr", "Hebrew", "Hira", "Hiragana", "Hluw", "Anatolian_Hieroglyphs",
		"Hmng", "Pahawh_Hmong", "Hmnp", "Nyiakeng_Puachue_Hmong", "Hung", "Old_Hungarian", "Ital", "Old_Italic", "Java", "Javanese", "Kali", "Kayah_Li", "Kana", "Katakana",
		"Kawi", "Khar", "Kharoshthi", "Khmr", "Khmer", "Khoj", "Khojki", "Kits", "Khitan_Small_Script", "Knda", "Kannada", "Krai", "Kirat_Rai", "Kthi", "Kaithi", "Lana", "Tai_Tham",
		"Laoo", "Lao", "Latn", "Latin", "Lepc", "Lepcha", "Limb", "Limbu", "Lina", "Linear_A", "Linb", "Linear_B", "Lisu", "Lyci", "Lycian", "Lydi", "Lydian", "Mahj", "Mahajani",
		"Maka", "Makasar", "Mand", "Mandaic", "Mani", "Manichaean", "Marc", "Marchen", "Medf", "Medefaidrin", "Mend", "Mende_Kikakui", "Merc", "Meroitic_Cursive", "Mero", "Meroitic_Hieroglyphs",
		"Mlym", "Malayalam", "Modi", "Mong", "Mongolian", "Mroo", "Mro", "Mtei", "Meetei_Mayek", "Mult", "Multani", "Mymr", "Myanmar", "Nagm", "Nag_Mundari", "Nand", "Nandinagari",
		"Narb", "Old_North_Arabian", "Nbat", "Nabataean", "Newa", "Nkoo", "Nko", "Nshu", "Nushu", "Ogam", "Ogham", "Olck", "Ol_Chiki", "Onao", "Ol_Onal", "Orkh", "Old_Turkic",
		"Orya", "Oriya", "Osge", "Osage", "Osma", "Osmanya", "Ougr", "Old_Uyghur", "Palm", "Palmyrene", "Pauc", "Pau_Cin_Hau", "Perm", "Old_Permic", "Phag", "Phags_Pa",
		"Phli", "Inscriptional_Pahlavi", "Phlp", "Psalter_Pahlavi", "Phnx", "Phoenician", "Plrd", "Miao", "Prti", "Inscriptional_Parthian", "Rjng", "Rejang", "Rohg", "Hanifi_Rohingya",
		"Runr", "Runic", "Samr", "Samaritan", "Sarb", "Old_South_Arabian", "Saur", "Saurashtra", "Sgnw", "SignWriting", "Shaw", "Shavian", "Shrd", "Sharada", "Sidd", "Siddham",
		"Sind", "Khudawadi", "Sinh", "Sinhala", "Sogd", "Sogdian", "Sogo", "Old_Sogdian", "Sora", "Sora_Sompeng", "Soyo", "Soyombo", "Sund", "Sundanese", "Sunu", "Sunuwar",
		"Sylo", "Syloti_Nagri", "Syrc", "Syriac", "Tagb", "Tagbanwa", "Takr", "Takri", "Tale", "Tai_Le", "Talu", "New_Tai_Lue", "Taml", "Tamil", "Tang", "Tangut", "Tavt", "Tai_Viet",
		"Telu", "Telugu", "Tfng", "Tifinagh", "Tglg", "Tagalog", "Thaa", "Thaana", "Thai", "Tibt", "Tibetan", "Tirh", "Tirhuta", "Tnsa", "Tangsa", "Todr", "Todhri", "Toto", "Tutg", "Tulu_Tigalari",
		"Ugar", "Ugaritic", "Vaii", "Vai", "Vith", "Vithkuqi", "Wara", "Warang_Citi", "Wcho", "Wancho", "Xpeo", "Old_Persian", "Xsux", "Cuneiform", "Yezi", "Yezidi", "Yiii", "Yi",
		"Zanb", "Zanabazar_Square", "Zinh", "Inherited", "Qaai", "Zyyy", "Common", "Zzzz", "Unknown":
		return true
	}
	return false
}

// Unicode 17.0 additions to ID_Continue compared with 16.0, from the two
// DerivedCoreProperties.txt files. ID_Start's additions are a subset.
func identifierChar(r rune, first bool) bool {
	if r == '$' || r == '_' {
		return true
	}
	for _, p := range [...][2]rune{{0x88f, 0x88f}, {0xc5c, 0xc5c}, {0xcdc, 0xcdc}, {0x1acf, 0x1add}, {0x1ae0, 0x1aeb}, {0xa7ce, 0xa7cf}, {0xa7d2, 0xa7d2}, {0xa7d4, 0xa7d4}, {0xa7f1, 0xa7f1}, {0x10940, 0x10959}, {0x10ec5, 0x10ec7}, {0x10efa, 0x10efb}, {0x11b60, 0x11b67}, {0x11db0, 0x11ddb}, {0x11de0, 0x11de9}, {0x16ea0, 0x16eb8}, {0x16ebb, 0x16ed3}, {0x16ff2, 0x16ff6}, {0x187f8, 0x187ff}, {0x18d09, 0x18d1e}, {0x18d80, 0x18df2}, {0x1e6c0, 0x1e6de}, {0x1e6e0, 0x1e6f5}, {0x1e6fe, 0x1e6ff}, {0x2b73a, 0x2b73f}, {0x2cea2, 0x2cead}, {0x323b0, 0x33479}} {
		if r >= p[0] && r <= p[1] {
			return false
		}
	}
	if unicode.In(r, unicode.Pattern_Syntax, unicode.Pattern_White_Space) {
		return false
	}
	if unicode.IsLetter(r) || unicode.In(r, unicode.Nl, unicode.Other_ID_Start) {
		return true
	}
	return !first && (r == '‌' || r == '‍' || unicode.In(r, unicode.Mn, unicode.Mc, unicode.Nd, unicode.Pc, unicode.Other_ID_Continue))
}
