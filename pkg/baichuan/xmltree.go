package baichuan

import (
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"strings"
)

// Node is a generic XML element. Configuration commands are read, changed
// and written back whole (GET -> modify -> SET), so elements this package
// does not know about must survive the round trip; structs would drop them.
type Node struct {
	Name     string
	Attrs    []xml.Attr
	Text     string
	Children []*Node
}

// ParseNode parses an XML document into its root element.
func ParseNode(s string) (*Node, error) {
	dec := xml.NewDecoder(strings.NewReader(s))
	var stack []*Node
	var root *Node
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			n := &Node{Name: t.Name.Local, Attrs: append([]xml.Attr(nil), t.Attr...)}
			if len(stack) > 0 {
				parent := stack[len(stack)-1]
				parent.Children = append(parent.Children, n)
			} else if root == nil {
				root = n
			}
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if len(stack) > 0 {
				stack[len(stack)-1].Text += string(t)
			}
		}
	}
	if root == nil {
		return nil, errors.New("baichuan: empty xml")
	}
	return root, nil
}

// Find returns the first descendant matching a slash-separated path of
// element names (case-insensitive, as the camera parses them), or nil.
func (n *Node) Find(path string) *Node {
	cur := n
	for _, name := range strings.Split(path, "/") {
		var next *Node
		for _, c := range cur.Children {
			if strings.EqualFold(c.Name, name) {
				next = c
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

// Value returns the trimmed text at path, or "" if it is missing.
func (n *Node) Value(path string) string {
	if c := n.Find(path); c != nil {
		return strings.TrimSpace(c.Text)
	}
	return ""
}

// Set changes the text at path if the element exists and reports whether the
// value changed. Missing elements are not created: a camera that does not
// report a setting does not support it.
func (n *Node) Set(path, value string) bool {
	c := n.Find(path)
	if c == nil || strings.TrimSpace(c.Text) == value {
		return false
	}
	c.Text = value
	return true
}

// Marshal writes the element as an XML document.
func (n *Node) Marshal() []byte {
	var b bytes.Buffer
	b.WriteString(xmlHeader)
	n.write(&b)
	b.WriteByte('\n')
	return b.Bytes()
}

const xmlHeader = "<?xml version=\"1.0\" encoding=\"UTF-8\" ?>\n"

func (n *Node) write(b *bytes.Buffer) {
	b.WriteByte('<')
	b.WriteString(n.Name)
	for _, a := range n.Attrs {
		b.WriteByte(' ')
		b.WriteString(a.Name.Local)
		b.WriteString(`="`)
		_ = xml.EscapeText(b, []byte(a.Value))
		b.WriteByte('"')
	}
	b.WriteByte('>')
	if len(n.Children) == 0 {
		_ = xml.EscapeText(b, []byte(strings.TrimSpace(n.Text)))
	} else {
		b.WriteByte('\n')
		for _, c := range n.Children {
			c.write(b)
			b.WriteByte('\n')
		}
	}
	b.WriteString("</")
	b.WriteString(n.Name)
	b.WriteByte('>')
}
