package main

import (
	"flag"
	"net/http"

	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"github.com/makeworld-the-better-one/dither/v2"
	"github.com/rileys-trash-can/libfp"
	"io"
	"log"
	"os"
	"runtime/debug"
	"strconv"
	"text/template"

	// image stuffs
	_ "github.com/samuel/go-pcx/pcx"
	_ "golang.org/x/image/bmp"
	"image"
	"image/color"
	_ "image/jpeg"
	"image/png"
)

var (
	//go:embed index.txt
	eIndexApi []byte

	//go:embed bs.css
	eBScss []byte

	//go:embed index.html
	eIndex []byte

	//go:embed jobstatus.template.html
	eStatus string

	tStatus *template.Template = func() *template.Template {
		templ, err := template.New("status").Parse(eStatus)
		if err != nil {
			panic(err)
		}

		return templ
	}()
)

var (
	ConfigPath = flag.String("config", "config.yml", "config to read")

	ListenAddr = flag.String("listen", "", "specify port to listen on, fallback is [::]:8070")

	PrinterAddressHost = flag.String("host", os.Getenv("IPL_PRINTER"), "Specify printer, can also be set by env IPL_PRINTER (net port)")
	PrinterAddressPort = flag.String("port", os.Getenv("IPL_PORT"), "Specify printer, can also be set by env IPL_PORT (usb port)")

	PrinterAddressType = flag.String("ctype", os.Getenv("IPL_CTYPE"), "Specify printer connection type, can also be set by env IPL_CTYPE")

	OptVerbose = flag.Bool("verbose", false, "toggle verbose logging")
	OptBeep    = flag.Bool("beep", true, "toggle connection-beep")
	OptDryRun  = flag.Bool("dry-run", false, "disables connection to printer; for testing")
)

var printer *fp.Printer

func main() {
	log.SetFlags(log.Flags() | log.Lshortfile)
	flag.Parse()
	conf := GetConfig()

	if !*OptDryRun {
		printer = OpenPrinter()
	}

	gmux := mux.NewRouter()

	// static stuff
	gmux.Path("/").
		Methods("GET").
		Handler(ErrorHandlerMiddleware(&handleFile{"text/html", eIndex}))

	gmux.Path("/bs.css").
		Methods("GET").
		Handler(ErrorHandlerMiddleware(&handleFile{"text/css", eBScss}))

	gmux.Path("/api").
		Methods("GET").
		Handler(ErrorHandlerMiddleware(&handleFile{"text/plain", eIndexApi}))

	// ui stuff
	gmux.Path("/img/{uuid}").
		Methods("GET").
		Handler(ErrorHandlerMiddleware(http.HandlerFunc(handleGetImg)))

	gmux.Path("/job/{uuid}").
		Methods("GET").
		Handler(ErrorHandlerMiddleware(http.HandlerFunc(handleJob)))

	gmux.Path("/api/print").
		Methods("POST").
		Handler(ErrorHandlerMiddleware(http.HandlerFunc(handlePrintPOST)))

	// api stuff
	gmux.Path("/api/print").
		Methods("PUT").
		Handler(ErrorHandlerMiddleware(http.HandlerFunc(handlePrint)))

	gmux.Path("/api/job/{uuid}").
		Methods("GET").
		Handler(ErrorHandlerMiddleware(http.HandlerFunc(handleJobAPI)))

	gmux.Path("/api/list").
		Methods("GET").
		Handler(ErrorHandlerMiddleware(http.HandlerFunc(handleList)))

	addr := T(*ListenAddr != "", *ListenAddr, conf.Listen)

	if addr == "" {
		addr = "[::]:8070"
	}

	log.Printf("Listening on %s", addr)
	log.Fatalf("Failed to ListenAndServe: %s",
		http.ListenAndServe(addr, gmux))
}

type handleFile struct {
	contenttype string
	data        []byte
}

func (hf *handleFile) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", hf.contenttype)
	w.WriteHeader(200)

	w.Write(hf.data)

	return
}

func handlePrint(w http.ResponseWriter, r *http.Request) {
	uid := uuid.New()
	newImageCh <- uid

	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(200)

	fmt.Fprintf(w, "job id: %s\n", uid)

	data, err := io.ReadAll(r.Body)
	if err != nil {
		imageUpdateCh <- Status{
			UUID:     uid,
			Step:     "Invalid File Upload: " + err.Error(),
			Progress: -1,
			Done:     true,
		}

		return
	}

	log.Printf("[POST] file %d bytes", len(data))

	q := r.URL.Query()

	job := &PrintJob{
		UUID: uid,

		public:     len(q["public"]) > 0,
		optresize:  len(q["resize"]) > 0,
		optstretch: len(q["stretch"]) > 0,
		optrotate:  len(q["rotate"]) > 0,
		optcenterh: len(q["centerh"]) > 0,
		optcenterv: len(q["centerv"]) > 0,
		opttiling:  len(q["tiling"]) > 0, //TODO: use
	}

	dname := ""
	dnames := q["dither"]

	log.Printf("%+v", q)
	if len(dnames) > 0 {
		dname = dnames[0]
	}

	job.ditherer = DitherFromString(dname)

	job.PFCount = 1
	pfs := q["pf"]
	if len(pfs) > 0 {
		i, err := strconv.ParseUint(pfs[0], 10, 32)
		job.PFCount = uint(i)
		if err != nil {
			imageUpdateCh <- Status{
				UUID:     uid,
				Step:     "Invalid PF Count: " + err.Error(),
				Progress: -1,
				Done:     true,
			}

			return
		}
	}

	sizexs, sizeys := q["x"], q["y"]
	if len(sizexs) == 0 || len(sizeys) == 0 {
		imageUpdateCh <- Status{
			UUID:     uid,
			Step:     "No Size of Label Specified",
			Progress: -1,
			Done:     true,
		}

		return
	}

	x64, err := strconv.ParseUint(sizexs[0], 10, 31)
	if err != nil {
		imageUpdateCh <- Status{
			UUID:     uid,
			Step:     "Invalid width: " + err.Error(),
			Progress: -1,
			Done:     true,
		}
		return
	}

	y64, err := strconv.ParseUint(sizeys[0], 10, 31)
	if err != nil {
		imageUpdateCh <- Status{
			UUID:     uid,
			Step:     "Invalid height: " + err.Error(),
			Progress: -1,
			Done:     true,
		}
		return
	}

	job.LabelSize = image.Pt(int(x64), int(y64))

	// image handeling
	imgcfg, imgfmt, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		if err != nil {
			imageUpdateCh <- Status{
				UUID:     uid,
				Step:     "Failed to Decode Image (header): " + err.Error(),
				Progress: -1,
				Done:     true,
			}

			return
		}
	}

	job.UnprocessedImage = Image{
		UUID: uuid.New(),

		IsProcessed: false,
		Ext:         imgfmt,
		Data:        data,
		Public:      job.public,
		Name:        "",
	}

	GetDB().Create(&job.UnprocessedImage)

	select {
	case printQ <- job:
		break

	default:
		imageUpdateCh <- Status{
			UUID: uid,

			Step:     "print queue full",
			Progress: -1,
			Done:     true,
		}
	}
	log.Printf("[POST] Received Image in %s format bounds: %d x %d", imgfmt, imgcfg.Width, imgcfg.Height)
}

var ErrorHandlerMiddleware = mux.MiddlewareFunc(func(next http.Handler) http.Handler {
	return &errMiddleware{next}
})

type errMiddleware struct {
	next http.Handler
}

type ErrorRes struct {
	Error any `json:"error"`
}

func (e *ErrorRes) MarshalJSON() ([]byte, error) {
	return json.Marshal(ErrorRes{Error: fmt.Sprint(e.Error)})
}

func (m *errMiddleware) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		err := recover()
		if err == nil {
			return
		}

		log.Printf("Error in %s request of '%s': %s", r.Method, r.URL.Path, err)
		if *OptVerbose {
			log.Print("stacktrace from panic: \n" + string(debug.Stack()))
		}

		switch w.Header().Get("Content-Type") {
		case "application/json":
			w.Header().Set("Location", "/")

			w.WriteHeader(500)
			enc := json.NewEncoder(w)
			enc.Encode(&ErrorRes{
				Error: err,
			})
			return

		default:
			w.Header().Set("Content-Type", "text/plain")
			fallthrough

		case "text/plain":
			w.WriteHeader(500)
			fmt.Fprintf(w, "There was an error handeling your request: %s\n return to / to do stuff", err)
		}

	}()

	m.next.ServeHTTP(w, r)
}

type Filter interface {
	Apply(img image.Image) image.Image
}

type PixMapperFilter struct {
	mapper *dither.Ditherer
}

func (mf *PixMapperFilter) Apply(src image.Image) image.Image {
	return mf.mapper.Dither(src)
}

func DitherFromString(n string) Filter {
	switch n {
	case "o4x4": // to dither
		mapper := dither.NewDitherer([]color.Color{color.White, color.Black})
		mapper.Mapper = dither.PixelMapperFromMatrix(dither.ClusteredDot4x4, 1.0)

		return &PixMapperFilter{mapper: mapper}

	case "noise": // to dither
		mapper := dither.NewDitherer([]color.Color{color.White, color.Black})
		mapper.Mapper = dither.RandomNoiseGrayscale(.1, .5)

		return &PixMapperFilter{mapper: mapper}

	case "bayer": // to dither
		mapper := dither.NewDitherer([]color.Color{color.White, color.Black})
		mapper.Mapper = dither.Bayer(3, 3, .6)

		return &PixMapperFilter{mapper: mapper}
	}

	return nil
}

func BoolFromString(n string) bool {
	switch n {
	case "on":
		return true
	case "true":
		return true
	}

	return false
}

func handleJob(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	w.Header().Set("Content-Type", "text/html")

	id, ok := vars["uuid"]
	if !ok {
		panic("no id specified")
	}

	uid, err := uuid.Parse(id)
	if err != nil {
		panic(err)
	}

	status := GetStatus(uid)
	if status == nil {
		panic("Invalid Status // uid unknown")
	}

	err = tStatus.Execute(w, status)
	if err != nil {
		panic(err)
	}
}

func handleJobAPI(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	w.Header().Set("Content-Type", "application/json")

	id, ok := vars["uuid"]
	if !ok {
		panic("no id specified")
	}

	uid, err := uuid.Parse(id)
	if err != nil {
		panic(err)
	}

	status := GetStatus(uid)
	if status == nil {
		panic("Invalid Status")
	}

	w.WriteHeader(200)

	log.Printf("[GET] /api/status/%s\n%+v", id, status)

	enc := json.NewEncoder(w)
	err = enc.Encode(status)
	if err != nil {
		panic(err)
	}
}

func handlePrintPOST(w http.ResponseWriter, r *http.Request) {
	uid := uuid.New()
	newImageCh <- uid

	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(200)

	fmt.Fprintf(w, `<head>
	  <meta http-equiv="Refresh" content="0; URL=/job/%s" />
	</head>`, uid)

	file, header, err := r.FormFile("file")
	if err != nil {
		imageUpdateCh <- Status{
			UUID:     uid,
			Step:     "Invalid File Upload: " + err.Error(),
			Progress: -1,
			Done:     true,
		}

		return
	}

	defer file.Close()

	log.Printf("[POST] file '%s' %d bytes", header.Filename, header.Size)

	job := &PrintJob{
		UUID: uid,

		ditherer: DitherFromString(r.FormValue("dither")),

		public:     BoolFromString(r.FormValue("public")),
		optresize:  BoolFromString(r.FormValue("resize")),
		optstretch: BoolFromString(r.FormValue("stretch")),
		optrotate:  BoolFromString(r.FormValue("rotate")),
		optcenterh: BoolFromString(r.FormValue("centerh")),
		optcenterv: BoolFromString(r.FormValue("centerv")),
		opttiling:  BoolFromString(r.FormValue("tiling")), //TODO: use
	}

	job.PFCount = 1
	if len(r.Form["pf"]) > 0 {
		i, err := strconv.ParseUint(r.FormValue("pf"), 10, 32)
		job.PFCount = uint(i)
		if err != nil {
			imageUpdateCh <- Status{
				UUID:     uid,
				Step:     "Invalid PF Count: " + err.Error(),
				Progress: -1,
				Done:     true,
			}

			return
		}
	}

	sizexs, sizeys := r.FormValue("x"), r.FormValue("y")
	if len(sizexs) == 0 || len(sizeys) == 0 {
		imageUpdateCh <- Status{
			UUID:     uid,
			Step:     "No Size of Label Specified",
			Progress: -1,
			Done:     true,
		}

		return
	}

	x64, err := strconv.ParseUint(sizexs, 10, 32)
	if err != nil {
		imageUpdateCh <- Status{
			UUID:     uid,
			Step:     "Invalid width: " + err.Error(),
			Progress: -1,
			Done:     true,
		}
		return
	}

	y64, err := strconv.ParseUint(sizeys, 10, 32)
	if err != nil {
		imageUpdateCh <- Status{
			UUID:     uid,
			Step:     "Invalid height: " + err.Error(),
			Progress: -1,
			Done:     true,
		}
		return
	}

	job.LabelSize = image.Pt(int(x64), int(y64))

	// image handeling
	data, err := io.ReadAll(file)
	if err != nil {
		imageUpdateCh <- Status{
			UUID:     uid,
			Step:     "Failed to Read Image: " + err.Error(),
			Progress: -1,
			Done:     true,
		}

		return
	}

	imgcfg, imgfmt, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		if err != nil {
			imageUpdateCh <- Status{
				UUID:     uid,
				Step:     "Failed to Decode Image (header): " + err.Error(),
				Progress: -1,
				Done:     true,
			}

			return
		}
	}

	job.UnprocessedImage = Image{
		UUID: uuid.New(),

		IsProcessed: false,
		Ext:         imgfmt,
		Data:        data,
		Public:      job.public,
		Name:        header.Filename,
	}

	GetDB().Create(&job.UnprocessedImage)

	select {
	case printQ <- job:
		break

	default:
		imageUpdateCh <- Status{
			UUID: uid,

			Step:     "print queue full",
			Progress: -1,
			Done:     true,
		}
	}
	log.Printf("[POST] Received Image in %s format bounds: %d x %d", imgfmt, imgcfg.Width, imgcfg.Height)
}

func b64image(img image.Image) string {
	b := &bytes.Buffer{}
	err := png.Encode(b, img)
	if err != nil {
		panic(err)
	}

	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}

type ImageList struct {
	Offset int `json:"offset"`
	Limit  int `json:"limit"`
	Total  int `json:"total"`

	Images []Image `json:"images"`
}

func handleList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	q := r.URL.Query()

	var (
		optall = len(q["all"]) > 0

		offsetstr = q["offset"]
		limitstr  = q["limit"]

		processed     = q["processed"]
		processedType = false
	)

	if len(processed) > 0 {
		processedType = BoolFromString(processed[0])
	}

	enc := json.NewEncoder(w)

	db := GetDB().Model(&Image{})

	if optall {
		var length int
		const limit = 10

		db.Select("count(1)").Find(&length)
		db = db.Select("UUID", "UnProcessed", "Processed",
			"IsProcessed", "Ext", "Public", "Name")

		if len(processed) > 0 {
			db = db.Where("is_processed", processedType)
		}

		var images []Image

		for i := 0; i < length; i += limit {
			db.Offset(i).Limit(limit).Find(&images)

			for k := 0; k < len(images); k++ {
				err := enc.Encode(images[k])
				if err != nil {
					panic(err)
				}
			}
		}
	} else {
		if len(offsetstr) == 0 || len(limitstr) == 0 {
			panic("Invalid or missing offset or limit!")
		}

		offset, err := strconv.ParseUint(offsetstr[0], 10, 31)
		if err != nil {
			panic(err)
		}

		limit, err := strconv.ParseUint(limitstr[0], 10, 31)
		if err != nil {
			panic(err)
		}

		if limit > 100 {
			panic("Invalid limit; limit > 100")
		}

		var length int
		db.Select("count(1)").Find(&length)

		var l = ImageList{
			Offset: int(offset),
			Limit:  int(limit),
			Total:  length,
		}
		db.Select("UUID", "UnProcessed", "Processed",
			"IsProcessed", "Ext", "Public", "Name").Offset(int(offset)).Limit(int(limit)).Find(&l.Images)

		err = enc.Encode(&l)
		if err != nil {
			panic(err)
		}
	}
}

func handleGetImg(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	conf := GetConfig()

	v := mux.Vars(r)
	t, ok := v["uuid"]
	if !ok {
		panic("no UUID")
	}

	uid, err := uuid.Parse(t)
	if err != nil {
		panic(err)
	}

	log.Printf("Serving image %s", conf.Saves+uid.String())
	img := GetImage(uid)

	w.Write(img.Data)
}

func T[K any](c bool, a, b K) K {
	if c {
		return a
	}

	return b
}
