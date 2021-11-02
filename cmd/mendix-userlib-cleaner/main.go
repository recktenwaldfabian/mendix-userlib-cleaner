package main

import (
	"fmt"
	"io/ioutil"
	"regexp"
	"strings"

	"archive/zip"

	"flag"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/op/go-logging"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

var log = logging.MustGetLogger("main")

var format = logging.MustStringFormatter(
	`%{color}%{time:15:04:05.000} %{shortfunc} ▶ %{level:.4s} %{id:03x}%{color:reset} %{message}`,
)

type JarProperties struct {
	version       string
	versionNumber int
	filePath      string
	fileName      string
	packageName   string
	name          string
	vendor        string
	license       string
}

func main() {

	flag.String("target", ".", "Path to userlib.")
	flag.Bool("clean", false, "Turn on to actually remove the duplicate JARs.")
	flag.Bool("verbose", false, "Turn on to see debug information.")
	flag.String("mode", "auto", "Jar parsing mode. Supported options: auto, strict")

	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.Parse()
	viper.BindPFlags(pflag.CommandLine)

	targetDir := viper.GetString("target")
	mode := viper.GetString("mode")
	clean := viper.GetBool("clean")
	verbose := viper.GetBool("verbose")

	backend := logging.NewLogBackend(os.Stderr, "", 0)
	backendFormatter := logging.NewBackendFormatter(backend, format)

	// Set the backends to be used.
	logging.SetBackend(backendFormatter)
	if verbose {
		logging.SetLevel(logging.DEBUG, "main")
	} else {
		logging.SetLevel(logging.INFO, "main")
	}

	jars := listAllJars(targetDir, mode)
	keepJars := computeJarsToKeep(jars)
	count := cleanJars(clean, jars, keepJars)

	if clean {
		log.Infof("Total files removed: %d", count)
	} else {
		log.Infof("Would have removed: %d files", count)
		log.Infof("Use --clean to actually remove above file(s)")
	}

}

func listAllJars(targetDir string, mode string) []JarProperties {
	log.Info("Finding and parsing JARs")
	files, err := ioutil.ReadDir(targetDir)
	if err != nil {
		log.Fatal(err)
	}
	jars := []JarProperties{}
	for _, f := range files {
		if strings.HasSuffix(f.Name(), ".jar") {
			log.Debugf("Processing JAR: %v", f.Name())
			filePath := filepath.Join(targetDir, f.Name())
			jarProp := getJarProps(filePath, mode)
			if strings.Compare(jarProp.filePath, "") != 0 {
				jars = append(jars, jarProp)
			}
		}
	}
	return jars
}

func getJarProps(filePath string, mode string) JarProperties {

	archive, err := zip.OpenReader(filePath)
	if err != nil {
		panic(err)
	}
	defer archive.Close()

	for _, f := range archive.File {
		fileName := filepath.Base(f.Name)

		if !(strings.Compare(f.Name, "META-INF/MANIFEST.MF") == 0 || strings.Compare(fileName, "pom.properties") == 0) {
			continue
		}
		//log.Println("unzipping file ", fileName)

		file, err := ioutil.TempFile("", "jar")
		if err != nil {
			log.Fatal(err)
		}
		defer os.Remove(file.Name())

		dstFile, err := os.OpenFile(file.Name(), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
		if err != nil {
			log.Fatal(err)
		}

		fileInArchive, err := f.Open()
		if err != nil {
			log.Fatal(err)
		}

		if _, err := io.Copy(dstFile, fileInArchive); err != nil {
			log.Fatal(err)
		}

		dstFile.Close()
		fileInArchive.Close()

		b, err := ioutil.ReadFile(file.Name())
		if err != nil {
			log.Warningf("Unable to read file: %v", err)
		}

		// try manifest first
		text := string(b)
		jar1 := parseManifest(filePath, text)
		if jar1.packageName != "" {
			log.Debugf("Parsed properties from MANIFEST: %v", jar1)
			return jar1
		}
		jar2 := parsePOM(filePath, text)
		if jar2.packageName != "" {
			log.Debugf("Parsed properties from POM: %v", jar2)
			return jar2
		}
	}

	if mode == "auto" {
		jar3 := parseOptimistic(filePath)
		if jar3.packageName != "" {
			log.Debugf("Parsed properties optimistically: %v", jar3)
			return jar3
		}
	}

	log.Warningf("Failed to parse metadata from %v", filePath)

	return JarProperties{filePath: ""}
}

func parseManifest(filePath string, text string) JarProperties {
	lines := strings.Split(text, "\n")
	jarProp := JarProperties{filePath: filePath, packageName: "", fileName: filepath.Base(filePath), version: ""}
	for _, line := range lines {
		line = strings.TrimSpace(line)
		pair := strings.Split(line, ": ")

		if len(pair) < 2 {
			continue
		}

		key := pair[0]
		value := pair[1]
		if key == "Bundle-SymbolicName" || key == "Extension-Name" {
			jarProp.packageName = value
		} else if key == "Bundle-Version" || key == "Implementation-Version" {
			jarProp.version = value
			jarProp.versionNumber = convertVersionToNumber(jarProp.version)
		} else if key == "Bundle-Vendor" || key == "Implementation-Vendor" {
			jarProp.vendor = value
		} else if key == "Bundle-License" {
			jarProp.license = value
		} else if key == "Bundle-Name" || key == "Implementation-Title" {
			jarProp.name = value
			jarProp.packageName = value
		}
	}
	return jarProp
}

func parsePOM(filePath string, text string) JarProperties {
	lines := strings.Split(text, "\n")
	jarProp := JarProperties{filePath: filePath, packageName: "", fileName: filepath.Base(filePath)}
	groupId := ""
	artifactId := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		pair := strings.Split(line, "=")
		if pair[0] == "groupId" {
			groupId = pair[1]
		} else if pair[0] == "artifactId" {
			artifactId = pair[1]
		} else if pair[0] == "version" {
			jarProp.version = pair[1]
			jarProp.versionNumber = convertVersionToNumber(jarProp.version)
		}
	}
	if groupId != "" && artifactId != "" {
		jarProp.packageName = groupId + "." + artifactId
	}
	return jarProp
}

func parseOptimistic(filePath string) JarProperties {
	// filePath = junit-4.11.jar
	jarProp := JarProperties{filePath: filePath, packageName: "", fileName: filepath.Base(filePath)}

	// version
	tokens := strings.Split(filePath, "-")
	if len(tokens) > 1 {
		jarProp.version = strings.Replace(tokens[len(tokens)-1], ".jar", "", 1)
		jarProp.versionNumber = convertVersionToNumber(jarProp.version)
	}

	archive, err := zip.OpenReader(filePath)
	if err != nil {
		panic(err)
	}
	defer archive.Close()
	re := regexp.MustCompile(`(org|com)/.*\.class$`)

	for _, f := range archive.File {
		if match := re.MatchString(f.Name); match {
			tokens = strings.Split(f.Name, "/")
			if len(tokens) > 3 {
				// eg. org/example/hello/MyClass.class
				tokens = tokens[:3]
			} else if len(tokens) > 2 {
				// eg. org/example/MyClass.class
				tokens = tokens[:2]
			} else {
				tokens = tokens[:1]
			}
			jarProp.packageName = strings.Join(tokens, ".")
			break
		}
	}
	return jarProp
}

func computeJarsToKeep(jars []JarProperties) map[string]JarProperties {
	log.Info("Computing duplicates")
	var keepJars = make(map[string]JarProperties)

	for _, jar1 := range jars {
		//log.Println("Checking " + jar1.filePath)
		if _, ok := keepJars[jar1.packageName]; !ok {
			keepJars[jar1.packageName] = jar1
		}
		packageName := jar1.packageName

		// find latest
		for _, jar2 := range jars {
			latestJar := keepJars[packageName]
			if strings.Compare(jar1.filePath, jar2.filePath) == 0 {
				// skip self
				continue
			}
			if strings.Compare(latestJar.filePath, jar2.filePath) == 0 {
				// skip self
				continue
			}
			if strings.Compare(packageName, jar2.packageName) == 0 {
				goodFileSuffix := fmt.Sprintf("%s%s", jar2.version, ".jar")
				if latestJar.versionNumber == jar2.versionNumber && strings.HasSuffix(jar2.filePath, goodFileSuffix) {
					log.Infof("Preferring file %v over %v", jar2.fileName, latestJar.fileName)
					keepJars[packageName] = jar2
				} else if latestJar.versionNumber < jar2.versionNumber {
					log.Infof("Found newer %v over %v", jar2.fileName, latestJar.fileName)
					keepJars[packageName] = jar2
				}
			}
		}
	}
	return keepJars
}

func cleanJars(remove bool, jars []JarProperties, keepJars map[string]JarProperties) int {
	log.Info("Cleaning...")
	count := 0
	for _, jar := range jars {
		jarToKeep := keepJars[jar.packageName]
		if strings.Compare(jar.filePath, jarToKeep.filePath) != 0 {
			if _, err := os.Stat(jar.filePath); err == nil {
				if remove {
					log.Warningf("Removing duplicate of %v: %v", jar.packageName, jar.fileName)
					os.Remove(jar.filePath)
				} else {
					log.Warningf("Would remove duplicate of %v: %v", jar.packageName, jar.fileName)
				}
				count++
			}
		} else {
			log.Debugf("Keeping jar: %v", jar)
		}
	}
	return count
}

func convertVersionToNumber(version string) int {
	// naive implementation. Feel free to suggest improvements

	re := regexp.MustCompile("[0-9]+")

	multiplier := 1000
	number := 0
	for _, c := range re.FindAllString(version, -1) {
		t, _ := strconv.Atoi(c)
		if number > 0 {
			number = number * multiplier
		}
		number += t
	}
	return number
}
