package main

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/hashicorp/go-retryablehttp"
	"go.uber.org/zap"
)

var filename = ""        // flag.String("f", "", ".odm file")
var outputDirectory = "" // flag.String("o", ".", "output directory")
var makeOutputDir = flag.Bool("m", false, "Make a unique output directory")
var dryRun = flag.Bool("d", false, "Dry run, don't download anything")
var log *zap.SugaredLogger

const ClientId = "00000000-0000-0000-0000-000000000000"
const HashSecret = "ELOSNOC*AIDEM*EVIRDREVO"
const OMC = "1.2.0"
const OS = "10.14.2"
const UserAgent = "OverDrive Media Console"

type OverDriveMedia struct {
	AcquisitionUrl Url      `xml:"License>AcquisitionUrl"`
	ContentId      string   `xml:"id,attr"`
	Formats        []Format `xml:"Formats>Format"`
	Metadata       string   `xml:",cdata"`
}

type Creator struct {
	Role   string `xml:"role,attr"`
	FileAs string `xml:"file-as,attr"`
	Name   string `xml:",innerxml"`
}

type Metadata struct {
	ContentType  string    `xml:"ContentType"`
	Title        string    `xml:"Title"`
	SortTitle    string    `xml:"SortTitle"`
	Publisher    string    `xml:"Publisher"`
	ThumbnailUrl string    `xml:"ThumbnailUrl"`
	CoverUrl     string    `xml:"CoverUrl"`
	Creators     []Creator `xml:"Creators>Creator"`
	Description  string    `xml:"Description"`
}

type Format struct {
	Parts     Parts      `xml:"Parts"`
	Protocols []Protocol `xml:"Protocols>Protocol"`
	Name      string     `xml:"name,attr"`
}

type Protocol struct {
	Method  string `xml:"method,attr"`
	BaseUrl string `xml:"baseurl,attr"`
}

type Parts struct {
	Count int    `xml:"count,attr"`
	Part  []Part `xml:"Part"`
}

type Part struct {
	Filename string `xml:"filename,attr"`
	Name     string `xml:"name,attr"`
	Number   uint   `xml:"number,attr"`
	Duration string `xml:"duration,attr"`
}

type Url struct {
	Value *url.URL
}

// ParseFlags parses the command line args, allowing flags to be
// specified after positional args.
func ParseFlags() error {
	return ParseFlagSet(flag.CommandLine, os.Args[1:])
}

// ParseFlagSet works like flagset.Parse(), except positional arguments are not
// required to come after flag arguments.
func ParseFlagSet(flagset *flag.FlagSet, args []string) error {
	var positionalArgs []string
	for {
		if err := flagset.Parse(args); err != nil {
			return err
		}
		// Consume all the flags that were parsed as flags.
		args = args[len(args)-flagset.NArg():]
		if len(args) == 0 {
			break
		}
		// There's at least one flag remaining and it must be a positional arg since
		// we consumed all args that were parsed as flags. Consume just the first
		// one, and retry parsing, since subsequent args may be flags.
		positionalArgs = append(positionalArgs, args[0])
		args = args[1:]
	}
	// Parse just the positional args so that flagset.Args()/flagset.NArgs()
	// return the expected value.
	// Note: This should never return an error.
	return flagset.Parse(positionalArgs)
}

func (u *Url) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var value string
	err := d.DecodeElement(&value, &start)
	if err != nil {
		return err
	}

	u.Value, err = url.Parse(value)
	if err != nil {
		// logger.Errorf(err)
		return err
	}

	return nil
}

func main() {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()
	log = logger.Sugar()

	flag.Parse()
	outputDirectory = flag.Arg(0)
	filename = flag.Arg(1)
	// log.Fatalw("done", "m", makeOutputDir)

	err := run()
	if err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if filename == "" {
		return errors.New("odm file required")
	}

	licenseValue := fmt.Sprintf("%s|%s|%s|%s", ClientId, OMC, OS, HashSecret)
	encodedLicenseValue := utf16.Encode([]rune(licenseValue))

	hash := sha1.New()
	binary.Write(hash, binary.LittleEndian, encodedLicenseValue)

	licenseHash := hash.Sum(nil)
	encodedLicenseHash := base64.StdEncoding.EncodeToString(licenseHash)

	file, err := os.Open(filename)
	if err != nil {
		log.Errorf("Failed to open %s: %e", file, err)
		return err
	}

	defer file.Close()
	if !*dryRun {
		defer os.Remove(filename)
	}

	odmDecoder := xml.NewDecoder(file)
	data := OverDriveMedia{}
	err = odmDecoder.Decode(&data)
	if err != nil {
		log.Errorf("Failed to decode: %e", err)
		return err
	}
	metadataReader := strings.NewReader(data.Metadata)
	metadataDecoder := xml.NewDecoder(metadataReader)
	metadata := Metadata{}
	err = metadataDecoder.Decode(&metadata)
	if err != nil {
		log.Errorf("Failed to decode metadata: %e", err)
		return err
	}

	if len(data.Formats) != 1 {
		return fmt.Errorf("expected 1 format, got %d", len(data.Formats))
	}

	if len(data.Formats[0].Parts.Part) != data.Formats[0].Parts.Count {
		return fmt.Errorf("expected %d format, got %d",
			data.Formats[0].Parts.Count, len(data.Formats))
	}

	if len(data.Formats[0].Protocols) != 1 {
		return fmt.Errorf("expected 1 protocol, got %d",
			len(data.Formats[0].Protocols))
	}

	if data.Formats[0].Protocols[0].Method != "download" {
		return fmt.Errorf("unknown protocol method: %s",
			data.Formats[0].Protocols[0].Method)
	}

	if *dryRun {
		log.Infow("data", "data", data)
		log.Infow("metadata", "metadata", metadata)
		os.Exit(0)
	}

	log.Infow("Downloading",
		"title", metadata.Title,
		"ContentType", metadata.ContentType)

	acquisitionUrl := data.AcquisitionUrl.Value
	acquisitionUrl.RawQuery = url.Values{
		"MediaID":  []string{data.ContentId},
		"ClientID": []string{ClientId},
		"OMC":      []string{OMC},
		"OS":       []string{OS},
		"Hash":     []string{encodedLicenseHash},
	}.Encode()

	request, err := http.NewRequest("GET", acquisitionUrl.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", UserAgent)

	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}

	if response.StatusCode != http.StatusOK {
		responseBody, err := io.ReadAll(response.Body)
		if err != nil {
			return err
		}
		return fmt.Errorf("acquiring license returned a %d status: %s",
			response.StatusCode, responseBody)
	}

	license, err := io.ReadAll(response.Body)
	log.Debugw("license acquired",
		"license", license,
		"MediaID", data.ContentId,
		"ClientId", ClientId,
		"OMC", OMC,
		"OS", OS,
		"Hash", encodedLicenseHash,
		"StatusCode", response.StatusCode)
	if err != nil {
		return err
	}

	log.Infow("selecting format",
		"name", data.Formats[0].Name,
	)

	var outDir string
	if *makeOutputDir {
		outDir = path.Join(outputDirectory, metadata.Title)
		log.Infow("creating output directory", "directory", outDir)
		os.MkdirAll(outDir, 0777)
	} else {
		log.Infow("output directory", "directory", outDir)
		outDir = outputDirectory
	}

	m3uFile, err := os.Create(path.Join(outDir, fmt.Sprintf("%s.m3u", metadata.Title)))
	if err != nil {
		log.Infow("error creating playlist",
			"err", err,
		)
	} else {
		defer m3uFile.Close()
	}

	m3uFile.WriteString("#EXTM3U\n#EXTENC:UTF-8\n")
	m3uFile.WriteString(fmt.Sprintf("#EXTALB:%s\n", metadata.Title))
	m3uFile.WriteString(fmt.Sprintf("#PLAYLIST:%s by %s\n", metadata.Title, metadata.Creators[0].Name))

	for _, creator := range metadata.Creators {
		m3uFile.WriteString(fmt.Sprintf("#EXTART:%s (%s)\n", creator.Name, creator.Role))
	}

	if metadata.CoverUrl != "" {
		ext := path.Ext(metadata.CoverUrl)
		coverPath := path.Join(outDir, fmt.Sprintf("cover%s", ext))
		err := downloadFile(metadata.CoverUrl, string(license), coverPath)
		if err != nil {
			log.Infow("error downloading cover",
				"err", err,
			)
		} else {
			m3uFile.WriteString(fmt.Sprintf("#EXTIMG:cover\ncover%s\n", ext))
		}
	}

	if metadata.ThumbnailUrl != "" {
		ext := path.Ext(metadata.ThumbnailUrl)
		thumbPath := path.Join(outDir, fmt.Sprintf("thumb%s", ext))
		err := downloadFile(metadata.ThumbnailUrl, string(license), thumbPath)
		if err != nil {
			log.Infow("error downloading thumb",
				"err", err,
			)
		} else {
			m3uFile.WriteString(fmt.Sprintf("#EXTIMG:thumbnail\nthumb%s\n", ext))
		}
	}

	for _, part := range data.Formats[0].Parts.Part {
		m3uFile.WriteString("\n")
		fileName := fmt.Sprintf("%s - %s.mp3", metadata.Title, part.Name)
		filePath := path.Join(outDir, fileName)

		log.Infow("downloading part...",
			"name", part.Name,
			"number", part.Number,
			"file", filePath,
		)

		partUrl := fmt.Sprintf("%s/%s", data.Formats[0].Protocols[0].BaseUrl, part.Filename)
		err := downloadFile(partUrl, string(license), filePath)
		if err != nil {
			log.Infow("error downloading",
				"err", err,
			)
			return err
		}
		duration, err := durationToSecs(part.Duration)
		if err != nil {
			log.Errorw("unable to interpret duration", "duration", part.Duration, "part", part.Name, "err", err)
			duration = 0
		}
		m3uFile.WriteString(fmt.Sprintf("#EXTINF:%d,%s - %s\n", duration, metadata.Title, part.Name))
		m3uFile.WriteString(fmt.Sprintf("%s\n", fileName))
	}

	return nil
}

func downloadFile(url string, license string, filePath string) error {
	request, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}

	request.Header.Set("ClientId", ClientId)
	request.Header.Set("License", license)
	request.Header.Set("User-Agent", UserAgent)

	retryClient := retryablehttp.NewClient()
	httpClient := retryClient.StandardClient()
	response, err := httpClient.Do(request)
	if err != nil {
		log.Errorw("error doing http request", "request", request, "err", err)
		return err
	}

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf(
			"downloading file returned a %d status", response.StatusCode)
	}

	file, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = io.Copy(file, response.Body)
	if err != nil {
		return err
	}

	return nil
}

func durationToSecs(duration string) (int, error) {
	parts := strings.Split(duration, ":")
	multipliers := []int{0, 60, 60 * 60, 60 * 60 * 24} // backwards
	total := 0
	for i := len(parts) - 1; i >= 0; i-- {
		v, err := strconv.Atoi(parts[i])
		if err != nil {
			log.Errorw("unable to parse duration", "i", i, "duration", duration)
			return 0, err
		}
		multiplier := multipliers[len(parts)-i-1]
		total = total + (v * multiplier)
		//log.Infow("durationToSecs", "parts", parts, "i", i, "v", v, "duration", duration, "multiplier", multiplier, "total", total, "multipliers_index", len(parts)-i)
	}
	return total, nil
}
