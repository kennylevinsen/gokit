package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/ioutil"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Preprocessor func(assets *Assets, path string, content []byte) (result []byte, err error)

type Assets struct {
	version              int
	baseURL              string
	lock                 sync.RWMutex
	preprocessors        map[string][]Preprocessor
	entries              map[string]*File
	byChecksum           map[string]*File
	templateCache        map[string]*template.Template
	templateCacheVersion int
	templateFuncMap      template.FuncMap
}

type File struct {
	path           string
	Content        []byte
	ContentGZipped []byte
	Hash           []byte
	HashString     string
	ContentType    string
}

func NewAssets(baseURL string) Assets {
	assets := Assets{
		version:              0,
		baseURL:              baseURL,
		preprocessors:        make(map[string][]Preprocessor),
		entries:              make(map[string]*File),
		byChecksum:           make(map[string]*File),
		templateCache:        make(map[string]*template.Template),
		templateCacheVersion: 0,
	}
	assets.templateFuncMap = template.FuncMap{
		"jscode": func(input string) template.JS { return template.JS(input) },
		"asset": func(virtualPath string) (string, error) {
			if virtualPath[0] != '/' {
				return "", errors.New("path argument must start with '/'")
			}
			return assets.GetUrl(virtualPath)
		},
		"assetinline": func(virtualPath string) (string, error) {
			if virtualPath[0] != '/' {
				return "", errors.New("path argument must start with '/'")
			}
			file, err := assets.Get(virtualPath)
			if err != nil {
				return "", err
			}
			return string(file.Content), nil
		},
	}

	assets.AddPreprocessor(".css", AssetCssPreprocessor)
	assets.AddPreprocessor(".css", AssetSourceMapPreprocessor)

	return assets
}

func (f *Assets) SetTemplateFunc(name string, templateFunc interface{}) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.templateFuncMap[name] = templateFunc
}

func (f *Assets) AddDirectory(directory string, virtualPath string) error {
	return filepath.Walk(directory, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			rel, err := filepath.Rel(directory, path)
			if err != nil {
				return err
			}

			f.AddFile(path, virtualPath+rel)
		}

		return err
	})
}

func (f *Assets) AddPreprocessor(extension string, processor Preprocessor) {
	f.lock.Lock()
	defer f.lock.Unlock()

	preprocessors := f.preprocessors[extension]
	if preprocessors == nil {
		preprocessors = make([]Preprocessor, 0, 10)
	}

	f.preprocessors[extension] = append(preprocessors, processor)
}

func (f *Assets) ClearPreprocessors(extension string) {
	f.lock.Lock()
	defer f.lock.Unlock()

	delete(f.preprocessors, extension)
}

func (f *Assets) AddFile(file string, virtualPath string) {
	f.lock.Lock()
	defer f.lock.Unlock()

	f.entries[virtualPath] = &File{
		path: file,
	}
	f.version++
}

func (f *Assets) Get(virtualPath string) (*File, error) {
	f.lock.RLock()
	file := f.entries[virtualPath]
	f.lock.RUnlock()
	if file == nil {
		return nil, errors.New("File Not Found: " + virtualPath)
	}

	if file.Content == nil {
		// read file content
		fileContent, err := ioutil.ReadFile(file.path)
		if err != nil {
			return nil, err
		}

		// figure out content type
		extension := filepath.Ext(file.path)
		file.ContentType = mime.TypeByExtension(extension)
		if file.ContentType == "" {
			file.ContentType = http.DetectContentType(fileContent)
		}

		// preprocess content
		f.lock.RLock()
		preprocessors := f.preprocessors[extension]
		f.lock.RUnlock()
		if preprocessors != nil {
			for _, processor := range preprocessors {
				newContent, err := processor(f, virtualPath, fileContent)
				if err != nil {
					return nil, err
				}

				fileContent = newContent
			}
		}

		// gzip content
		var buffer bytes.Buffer
		compressor := gzip.NewWriter(&buffer)
		compressor.Write(fileContent)
		compressor.Close()
		file.ContentGZipped = buffer.Bytes()

		// sha1 the content.
		h := sha1.New()
		h.Write(fileContent)
		file.Hash = h.Sum(nil)
		file.HashString = hex.EncodeToString(file.Hash)
		f.lock.Lock()
		f.byChecksum[file.HashString] = file
		f.lock.Unlock()

		// set the content (this is done last to minimize the chance of two goroutines in this if-statement)
		file.Content = fileContent
	}

	return file, nil
}

func (f *Assets) GetUrl(virtualPath string) (string, error) { //todo: returns /a/<checksum> w/ forever expires.
	file, err := f.Get(virtualPath)
	if err != nil {
		return "", err
	}

	return f.baseURL + file.HashString, nil
}

func (f *Assets) Serve(url string, w http.ResponseWriter, r *http.Request) {
	if len(url) < len(f.baseURL) {
		httpError(w, 404, "404 - File not found")
		return
	}

	checksum := url[len(f.baseURL):]
	f.lock.RLock()
	file := f.byChecksum[checksum]
	f.lock.RUnlock()

	if file == nil {
		httpError(w, 404, "404 - File not found")
		return
	}

	w.Header().Set("Content-Type", file.ContentType)
	w.Header().Set("Expires", time.Now().AddDate(1, 0, 0).Format(http.TimeFormat))

	if r != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(file.ContentGZipped)
	} else {
		w.Write(file.Content)
	}
}

func (f *Assets) RenderTemplate(templatePathArr []string, w http.ResponseWriter, data interface{}) error {
	return f.RenderNamedTemplate(templatePathArr, templatePathArr[len(templatePathArr)-1], w, data)
}

func (f *Assets) RenderNamedTemplate(templatePathArr []string, name string, w http.ResponseWriter, data interface{}) error {
	t, err := f.GetTemplate(templatePathArr)

	if err != nil {
		httpError(w, 500, err.Error())
		return err
	}

	err = t.ExecuteTemplate(w, name, data)
	if err != nil {
		httpError(w, 500, err.Error())
		return err
	}
	return nil
}

func (f *Assets) GetTemplate(templatePathArr []string) (*template.Template, error) {
	// reset cache if filesystem has changed
	if f.version != f.templateCacheVersion {
		f.lock.Lock()
		f.templateCacheVersion = f.version
		f.templateCache = make(map[string]*template.Template)
		f.lock.Unlock()
	}

	// check cache
	cacheKey := strings.Join(templatePathArr, "<")
	f.lock.RLock()
	tmpl := f.templateCache[cacheKey]
	f.lock.RUnlock()
	if tmpl != nil {
		return tmpl, nil
	}

	// not found in cache, create new.
	tmpl = template.New("temp-outer-template-shell").Funcs(f.templateFuncMap)

	for _, path := range templatePathArr {
		if path != "" {
			file, err := f.Get(path)
			if err != nil {
				return nil, err
			}

			temp, err := template.New(path).Funcs(f.templateFuncMap).Parse(string(file.Content))
			if err != nil {
				return nil, errors.New(path + ": " + err.Error())
			}

			for _, t := range temp.Templates() {
				if tmpl.Lookup(t.Name()) == nil {
					//fmt.Println("==================> "+t.Name()+" = "+path, string(file.content), t.Tree)
					tmpl.AddParseTree(t.Name(), t.Tree)
				}
			}
		}
	}

	f.lock.Lock()
	f.templateCache[cacheKey] = tmpl
	f.lock.Unlock()

	return tmpl, nil
}

func httpError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	fmt.Fprintln(w, message)
}

func (f *Assets) getRooted(fromFile string, targetFile string) (string, error) {
	if targetFile[0] == '/' {
		return targetFile, nil
	}
	return filepath.Join(filepath.Dir(fromFile), targetFile), nil
}

// ------
var cssUrlRegex = regexp.MustCompile(`url\([^\)]+\)`)
var sourceMapRegex = regexp.MustCompile(`sourceMappingURL=\S+`)

func AssetCssPreprocessor(assets *Assets, path string, content []byte) ([]byte, error) {
	return replaceProcessor(assets, path, content, cssUrlRegex, "url(", ")")
}

func AssetSourceMapPreprocessor(assets *Assets, path string, content []byte) ([]byte, error) {
	return replaceProcessor(assets, path, content, sourceMapRegex, "sourceMappingURL=", "")
}

func replaceProcessor(assets *Assets, path string, content []byte, regex *regexp.Regexp, prefix string, postfix string) ([]byte, error) {
	var replaceErr error = nil
	newContent := regex.ReplaceAllFunc(content, func(match []byte) []byte {
		//fmt.Println("Match: " + string(match))
		file := string(match)[len(prefix) : len(match)-len(postfix)]

		// root the path
		rootedPath, err := assets.getRooted(path, strings.TrimSpace(file))
		if err != nil {
			replaceErr = err
			return match
		}

		// inline base64 support
		/*base64encode := rootedPath == "/images/sprite.png"
		if base64encode {
			f, err := assets.Get(rootedPath)
			if err != nil {
				replaceErr = err
				return match
			}
			var buf bytes.Buffer
			buf.WriteString("data:")
			buf.WriteString(f.ContentType)
			buf.WriteString(";base64,")
			buf.WriteString(base64.StdEncoding.EncodeToString(f.Content))
			return buf.Bytes()
		}*/

		// get the url from asset system
		url, err := assets.GetUrl(rootedPath)
		if err != nil {
			replaceErr = err
			return match
		}
		return []byte(prefix + url + postfix)
	})

	if replaceErr != nil {
		return nil, replaceErr
	}

	return newContent, nil
}
