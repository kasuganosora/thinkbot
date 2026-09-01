package searchproviders

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"strings"
)

type xmlInnerText string

func (t *xmlInnerText) UnmarshalXML(d *xml.Decoder, _ xml.StartElement) error {
	var buf strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch v := tok.(type) {
		case xml.CharData:
			buf.Write(v)
		case xml.StartElement:
			var inner xmlInnerText
			if err := d.DecodeElement(&inner, &v); err != nil {
				return err
			}
			buf.WriteString(string(inner))
		case xml.EndElement:
			*t = xmlInnerText(buf.String())
			return nil
		}
	}
	*t = xmlInnerText(buf.String())
	return nil
}

type yandexResponse struct {
	XMLName xml.Name      `xml:"response"`
	Results yandexResults `xml:"results"`
}
type yandexResults struct {
	Grouping yandexGrouping `xml:"grouping"`
}
type yandexGrouping struct {
	Groups []yandexGroup `xml:"group"`
}
type yandexGroup struct {
	Doc yandexDoc `xml:"doc"`
}
type yandexDoc struct {
	URL      xmlInnerText   `xml:"url"`
	Title    xmlInnerText   `xml:"title"`
	Passages yandexPassages `xml:"passages"`
}
type yandexPassages struct {
	Passage []xmlInnerText `xml:"passage"`
}

func parseYandexRawData(rawData string) ([]Result, error) {
	xmlData, err := base64.StdEncoding.DecodeString(rawData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode Yandex response")
	}
	return parseYandexXML(xmlData)
}

func parseYandexXML(data []byte) ([]Result, error) {
	var resp yandexResponse
	if err := xml.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse Yandex XML response")
	}
	results := make([]Result, 0, len(resp.Results.Grouping.Groups))
	for _, group := range resp.Results.Grouping.Groups {
		snippet := ""
		if len(group.Doc.Passages.Passage) > 0 {
			snippet = string(group.Doc.Passages.Passage[0])
		}
		results = append(results, Result{
			Title:   string(group.Doc.Title),
			URL:     string(group.Doc.URL),
			Snippet: snippet,
		})
	}
	return results, nil
}
