package render

import (
	glamouransi "charm.land/glamour/v2/ansi"
	"charm.land/glamour/v2/styles"
)

func markdownStyle() glamouransi.StyleConfig {
	style := styles.DarkStyleConfig
	zero := uint(0)
	style.Document.Margin = &zero
	color := func(s string) *string { return &s }
	for _, block := range []*glamouransi.StyleBlock{&style.Document, &style.Paragraph, &style.CodeBlock.StyleBlock} {
		block.Color = color(TextColor)
		block.BackgroundColor = nil
	}
	for _, heading := range []*glamouransi.StyleBlock{&style.Heading, &style.H1, &style.H2, &style.H3, &style.H4, &style.H5, &style.H6} {
		heading.Color = color(CyanColor)
		heading.BackgroundColor = nil
	}
	style.Strong.Color = color(CyanColor)
	style.HorizontalRule.Color = color(BorderColor)
	style.Link.Color, style.LinkText.Color = color(BlueColor), color(BlueColor)
	style.Image.Color, style.ImageText.Color = color(BlueColor), color(MutedColor)
	style.Code.Color, style.Code.BackgroundColor = color(BlueColor), nil
	style.CodeBlock.Theme = ""
	plain := func(c string) glamouransi.StylePrimitive { return glamouransi.StylePrimitive{Color: color(c)} }
	style.CodeBlock.Chroma = &glamouransi.Chroma{
		Text: plain(TextColor), Error: plain(AmberColor),
		Comment: plain(MutedColor), CommentPreproc: plain(MutedColor),
		Keyword: plain(LavenderColor), KeywordReserved: plain(LavenderColor),
		KeywordNamespace: plain(LavenderColor), KeywordType: plain(LavenderColor),
		Operator: plain(TextColor), Punctuation: plain(MutedColor),
		Name: plain(TextColor), NameBuiltin: plain(BlueColor), NameTag: plain(BlueColor),
		NameAttribute: plain(BlueColor), NameClass: plain(BlueColor),
		NameConstant: plain(CyanColor), NameDecorator: plain(LavenderColor),
		NameException: plain(AmberColor), NameFunction: plain(BlueColor),
		LiteralNumber: plain(CyanColor), LiteralString: plain(AmberColor),
		LiteralStringEscape: plain(CyanColor), GenericDeleted: plain(AmberColor),
		GenericInserted: plain(BlueColor), GenericSubheading: plain(CyanColor),
		GenericEmph: style.Emph, GenericStrong: style.Strong,
	}
	return style
}
