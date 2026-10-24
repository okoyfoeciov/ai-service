package server

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
)

// multipartBody encodes one file part followed by ordered text fields. An empty
// fileType uses CreateFormFile, which escapes the filename; Azure's verbatim
// route passes the client's own filename.
func multipartBody(fileField, filename, fileType string, file []byte, fields ...[2]string) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	form := multipart.NewWriter(&body)

	var (
		part io.Writer
		err  error
	)
	if fileType == "" {
		part, err = form.CreateFormFile(fileField, filename)
	} else {
		part, err = form.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {fmt.Sprintf(`form-data; name=%q; filename=%q`, fileField, filename)},
			"Content-Type":        {fileType},
		})
	}
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(file); err != nil {
		return nil, "", err
	}
	for _, f := range fields {
		if err := form.WriteField(f[0], f[1]); err != nil {
			return nil, "", err
		}
	}
	if err := form.Close(); err != nil {
		return nil, "", err
	}
	return &body, form.FormDataContentType(), nil
}
