package main

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"github.com/bitrise-steplib/steps-deploy-to-bitrise-io/androidartifact"
	"github.com/bitrise-steplib/steps-deploy-to-bitrise-io/test"

	"github.com/bitrise-io/go-steputils/stepconf"
	"github.com/bitrise-io/go-steputils/tools"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-io/go-utils/pathutil"
	"github.com/bitrise-io/go-utils/ziputil"
	"github.com/bitrise-steplib/steps-deploy-to-bitrise-io/uploaders"
)

var fileBaseNamesToSkip = []string{".DS_Store"}

// Config ...
type Config struct {
	BuildURL                   string `env:"build_url,required"`
	APIToken                   string `env:"build_api_token,required"`
	IsCompress                 string `env:"is_compress,opt[true,false]"`
	ZipName                    string `env:"zip_name"`
	DeployPath                 string `env:"deploy_path,required"`
	NotifyUserGroups           string `env:"notify_user_groups"`
	NotifyEmailList            string `env:"notify_email_list"`
	IsPublicPageEnabled        string `env:"is_enable_public_page,opt[true,false]"`
	PublicInstallPageMapFormat string `env:"public_install_page_url_map_format,required"`
	BuildSlug                  string `env:"BITRISE_BUILD_SLUG,required"`
	TestDeployDir              string `env:"BITRISE_TEST_DEPLOY_DIR,required"`
	AppSlug                    string `env:"BITRISE_APP_SLUG,required"`
	AddonAPIBaseURL            string `env:"addon_api_base_url,required"`
	AddonAPIToken              string `env:"addon_api_token"`
	DebugMode                  bool   `env:"debug_mode,opt[true,false]"`
}

// PublicInstallPage ...
type PublicInstallPage struct {
	File string
	URL  string
}

const zippedXcarchiveExt = ".xcarchive.zip"

func fail(format string, v ...interface{}) {
	log.Errorf(format, v...)
	os.Exit(1)
}

func main() {
	var config Config
	if err := stepconf.Parse(&config); err != nil {
		fail("Issue with input: %s", err)
	}

	if err := validateGoTemplate(config.PublicInstallPageMapFormat); err != nil {
		fail("PublicInstallPageMapFormat - %s", err)
	}

	stepconf.Print(config)
	fmt.Println()
	log.SetEnableDebugLog(config.DebugMode)

	absDeployPth, err := pathutil.AbsPath(config.DeployPath)
	if err != nil {
		fail("Failed to expand path: %s, error: %s", config.DeployPath, err)
	}

	tmpDir, err := pathutil.NormalizedOSTempDirPath("__deploy-to-bitrise-io__")
	if err != nil {
		fail("Failed to create tmp dir, error: %s", err)
	}

	filesToDeploy, err := collectFilesToDeploy(absDeployPth, config, tmpDir)
	if err != nil {
		fail("%s", err)
	}
	clearedFilesToDeploy := clearDeployFiles(filesToDeploy)
	fmt.Println()
	log.Infof("List of files to deploy")
	logDeployFiles(clearedFilesToDeploy)

	fmt.Println()
	log.Infof("Deploying files")

	publicInstallPages, err := deploy(clearedFilesToDeploy, config)
	if err != nil {
		fail("%s", err)
	}
	fmt.Println()
	log.Donef("Success")
	log.Printf("You can find the Artifact on Bitrise, on the Build's page: %s", config.BuildURL)

	if err := exportInstallPages(publicInstallPages, config); err != nil {
		fail("%s", err)
	}
	deployTestResults(config)
}

func exportInstallPages(publicInstallPages map[string]string, config Config) error {
	if len(publicInstallPages) > 0 {
		temp := template.New("Public Install Page template")
		var pages []PublicInstallPage
		for file, url := range publicInstallPages {
			pages = append(pages, PublicInstallPage{
				File: file,
				URL:  url,
			})
		}

		if err := tools.ExportEnvironmentWithEnvman("BITRISE_PUBLIC_INSTALL_PAGE_URL", pages[0].URL); err != nil {
			return fmt.Errorf("failed to export BITRISE_PUBLIC_INSTALL_PAGE_URL, error: %s", err)
		}
		log.Printf("The public install page url is now available in the Environment Variable: BITRISE_PUBLIC_INSTALL_PAGE_URL (value: %s)\n", pages[0].URL)

		temp, err := temp.Parse(config.PublicInstallPageMapFormat)
		if err != nil {
			return fmt.Errorf("error during parsing PublicInstallPageMap: %s", err)
		}

		buf := new(bytes.Buffer)
		if err := temp.Execute(buf, pages); err != nil {
			return fmt.Errorf("execute: %s", err)
		}

		if err := tools.ExportEnvironmentWithEnvman("BITRISE_PUBLIC_INSTALL_PAGE_URL_MAP", buf.String()); err != nil {
			return fmt.Errorf("failed to export BITRISE_PUBLIC_INSTALL_PAGE_URL_MAP, error: %s", err)
		}
		log.Printf("A map of deployed files and their public install page urls is now available in the Environment Variable: BITRISE_PUBLIC_INSTALL_PAGE_URL_MAP (value: %s)", buf.String())
		log.Printf("")
	}
	return nil
}

func logDeployFiles(clearedFilesToDeploy []string) {
	for _, pth := range clearedFilesToDeploy {
		log.Printf("- %s", pth)
	}
}

func clearDeployFiles(filesToDeploy []string) []string {
	var clearedFilesToDeploy []string
	for _, pth := range filesToDeploy {
		for _, fileBaseNameToSkip := range fileBaseNamesToSkip {
			if filepath.Base(pth) == fileBaseNameToSkip {
				log.Warnf("skipping: %s", pth)
			} else {
				clearedFilesToDeploy = append(clearedFilesToDeploy, pth)
			}

		}
	}
	return clearedFilesToDeploy
}

func collectFilesToDeploy(absDeployPth string, config Config, tmpDir string) (filesToDeploy []string, err error) {
	isDeployPathDir, err := pathutil.IsDirExists(absDeployPth)
	if err != nil {
		return nil, fmt.Errorf("failed to check if DeployPath (%s) is a directory or a file, error: %s", absDeployPth, err)
	}

	if !isDeployPathDir {
		fmt.Println()
		log.Infof("Deploying single file")

		filesToDeploy = []string{absDeployPth}
	} else if config.IsCompress == "true" {
		fmt.Println()
		log.Infof("Deploying compressed Deploy directory")

		zipName := filepath.Base(absDeployPth)
		if config.ZipName != "" {
			zipName = config.ZipName
		}
		tmpZipPath := filepath.Join(tmpDir, zipName+".zip")

		if err := ziputil.ZipDir(absDeployPth, tmpZipPath, true); err != nil {
			return nil, fmt.Errorf("failed to zip output dir, error: %s", err)
		}

		filesToDeploy = []string{tmpZipPath}
	} else {
		fmt.Println()
		log.Infof("Deploying the content of the Deploy directory separately")

		pattern := filepath.Join(absDeployPth, "*")
		pths, err := filepath.Glob(pattern)
		if err != nil {
			return nil, fmt.Errorf("failed to list files in DeployPath, error: %s", err)
		}

		for _, pth := range pths {
			if isDir, err := pathutil.IsDirExists(pth); err != nil {
				return nil, fmt.Errorf("failed to check if path (%s) is a directory or a file, error: %s", pth, err)
			} else if !isDir {
				filesToDeploy = append(filesToDeploy, pth)
			}
		}
	}

	return filesToDeploy, nil
}

func deployTestResults(config Config) {
	if config.AddonAPIToken != "" {
		fmt.Println()
		log.Infof("Upload test results")

		testResults, err := test.ParseTestResults(config.TestDeployDir)
		if err != nil {
			log.Warnf("error during parsing test results: ", err)
		} else {
			log.Printf("- uploading (%d) test results", len(testResults))

			if err := testResults.Upload(config.AddonAPIToken, config.AddonAPIBaseURL, config.AppSlug, config.BuildSlug); err != nil {
				log.Warnf("Failed to upload test results: ", err)
			} else {
				log.Donef("Success")
			}
		}
	}
}

func findUniversalAPKPair(aab string, apks []string) string {
	universalAPKBase := androidartifact.UniversalAPKBase(aab)
	return androidartifact.FindSameArtifact(universalAPKBase, apks)
}

func ensureAABsHasUniversalAPKPair(aabs, apks []string) ([]string, map[string]string, error) {
	aabAPKPairs := map[string]string{}
	for _, aab := range aabs {
		log.Debugf("Looking for universal APK pair for: %s", aab)

		universalAPKPair := findUniversalAPKPair(aab, apks)
		if len(universalAPKPair) == 0 {
			log.Debugf("Does not have universal APK pair, generating...")

			var err error
			universalAPKPair, err = GenerateUniversalAPK(aab)
			if err != nil {
				return nil, nil, err
			}

			log.Debugf("Generated universal APK pair: %s", universalAPKPair)

			apks = append(apks, universalAPKPair)
		} else {
			log.Debugf("Universal APK pair: %s", universalAPKPair)
		}

		aabAPKPairs[aab] = universalAPKPair
	}
	return apks, aabAPKPairs, nil
}

func findAPKsAndAABs(pths []string) (apks []string, aabs []string, others []string) {
	for _, pth := range pths {
		switch getFileType(pth) {
		case ".apk":
			apks = append(apks, pth)
		case ".aab":
			aabs = append(aabs, pth)
		default:
			others = append(others, pth)
		}
	}
	return
}

func deploy(clearedFilesToDeploy []string, config Config) (map[string]string, error) {
	apks, aabs, others := findAPKsAndAABs(clearedFilesToDeploy)
	// Do not generate and distribute the universal apk - it is improperly signed:
	//apks, aabAPKPairs, err := ensureAABsHasUniversalAPKPair(aabs, apks)
	//if err != nil {
	//	return nil, err
	//}
	androidArtifacts := append(apks, aabs...)

	// deploy the apks first, as the universal apk's public install page will be used for aabs.
	publicInstallPages := map[string]string{}
	apkInstallPages := map[string]string{}
	for _, apk := range apks {
		log.Donef("Uploading apk file: %s", apk)

		installPage, err := uploaders.DeployAPK(apk, androidArtifacts, config.BuildURL, config.APIToken, config.NotifyUserGroups, config.NotifyEmailList, config.IsPublicPageEnabled)
		if err != nil {
			return nil, fmt.Errorf("deploy failed, error: %s", err)
		}

		if installPage != "" {
			publicInstallPages[filepath.Base(apk)] = installPage
			apkInstallPages[filepath.Base(apk)] = installPage
		}
	}

	for _, pth := range append(aabs, others...) {
		fileType := getFileType(pth)
		fmt.Println()

		switch fileType {
		case ".ipa":
			log.Donef("Uploading ipa file: %s", pth)

			installPage, err := uploaders.DeployIPA(pth, config.BuildURL, config.APIToken, config.NotifyUserGroups, config.NotifyEmailList, config.IsPublicPageEnabled)
			if err != nil {
				return nil, fmt.Errorf("deploy failed, error: %s", err)
			}

			if installPage != "" {
				publicInstallPages[filepath.Base(pth)] = installPage
			}
		case ".aab":
			log.Donef("Uploading aab file: %s", pth)

			if err := uploaders.DeployAAB(pth, androidArtifacts, config.BuildURL, config.APIToken,
				config.NotifyUserGroups, config.NotifyEmailList, config.IsPublicPageEnabled); err != nil {
				return nil, fmt.Errorf("deploy failed, error: %s", err)
			}

			universalAPKPair, ok := aabAPKPairs[pth]
			if !ok {
				return nil, fmt.Errorf("no iniversal apk pair found for aab: %s", pth)
			}

			if universalAPKInstallPage := apkInstallPages[filepath.Base(universalAPKPair)]; universalAPKInstallPage != "" {
				publicInstallPages[filepath.Base(pth)] = universalAPKInstallPage
			}
		case zippedXcarchiveExt:
			log.Donef("Uploading xcarchive file: %s", pth)
			if err := uploaders.DeployXcarchive(pth, config.BuildURL, config.APIToken); err != nil {
				return nil, fmt.Errorf("deploy failed, error: %s", err)
			}
		default:
			log.Donef("Uploading file: %s", pth)

			installPage, err := uploaders.DeployFile(pth, config.BuildURL, config.APIToken, config.NotifyUserGroups, config.NotifyEmailList, config.IsPublicPageEnabled)
			if err != nil {
				return nil, fmt.Errorf("deploy failed, error: %s", err)
			}

			if installPage != "" {
				publicInstallPages[filepath.Base(pth)] = installPage
			} else if config.IsPublicPageEnabled == "true" {
				log.Warnf("is_enable_public_page is set, but public download isn't allowed for this type of file")
			}
		}
	}
	return publicInstallPages, nil
}

func getFileType(pth string) string {
	if strings.HasSuffix(pth, zippedXcarchiveExt) {
		return zippedXcarchiveExt
	}
	return filepath.Ext(pth)
}

func validateGoTemplate(publicInstallPageMapFormat string) error {
	temp := template.New("Public Install Page Map template")

	_, err := temp.Parse(publicInstallPageMapFormat)
	return err
}
