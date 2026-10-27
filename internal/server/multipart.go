package server

import (
	"bytes"
	"fmt"
	"mime/multipart"
	"net/textproto"
)

// multipartBody encodes one file part with its declared media type.
func multipartBody(fileField, filename, fileType string, file []byte) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)

	part, err := form.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {fmt.Sprintf(`form-data; name=%q; filename=%q`, fileField, filename)},
		"Content-Type":        {fileType},
	})
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(file); err != nil {
		return nil, "", err
	}
	if err := form.Close(); err != nil {
		return nil, "", err
	}
	return &body, form.FormDataContentType(), nil
}
