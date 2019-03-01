/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package chaincode

import (
	"context"
	"fmt"
	"io/ioutil"

	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric/core/chaincode/persistence"
	"github.com/hyperledger/fabric/core/common/ccpackage"
	"github.com/hyperledger/fabric/core/common/ccprovider"
	"github.com/hyperledger/fabric/msp"
	"github.com/hyperledger/fabric/peer/common"
	cb "github.com/hyperledger/fabric/protos/common"
	pb "github.com/hyperledger/fabric/protos/peer"
	lb "github.com/hyperledger/fabric/protos/peer/lifecycle"
	"github.com/hyperledger/fabric/protoutil"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var chaincodeInstallCmd *cobra.Command

const (
	installCmdName = "install"
)

// Reader defines the interface needed for reading a file
type Reader interface {
	ReadFile(string) ([]byte, error)
}

// Installer holds the dependencies needed to install
// a chaincode
type Installer struct {
	Command         *cobra.Command
	EndorserClients []pb.EndorserClient
	Input           *InstallInput
	Reader          Reader
	Signer          msp.SigningIdentity
}

// InstallInput holds the input parameters for installing
// a chaincode
type InstallInput struct {
	Name         string
	Version      string
	Language     string
	PackageFile  string
	Path         string
	NewLifecycle bool
}

// installCmd returns the cobra command for chaincode install
func installCmd(cf *ChaincodeCmdFactory, i *Installer) *cobra.Command {
	chaincodeInstallCmd = &cobra.Command{
		Use:       "install",
		Short:     "Install a chaincode.",
		Long:      "Install a chaincode on a peer. For the legacy lifecycle (lscc), this installs a chaincode deployment spec package (if provided) or packages the specified chaincode before subsequently installing it.",
		ValidArgs: []string{"1"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if i == nil {
				var err error
				if cf == nil {
					cf, err = InitCmdFactory(cmd.Name(), true, false)
					if err != nil {
						return err
					}
				}
				i = &Installer{
					Command:         cmd,
					EndorserClients: cf.EndorserClients,
					Reader:          &persistence.FilesystemIO{},
					Signer:          cf.Signer,
				}
			}
			return i.installChaincode(args)
		},
	}
	flagList := []string{
		"lang",
		"ctor",
		"path",
		"name",
		"version",
		"peerAddresses",
		"tlsRootCertFiles",
		"connectionProfile",
		"newLifecycle",
	}
	attachFlags(chaincodeInstallCmd, flagList)

	return chaincodeInstallCmd
}

// installChaincode installs the chaincode
func (i *Installer) installChaincode(args []string) error {
	if i.Command != nil {
		// Parsing of the command line is done so silence cmd usage
		i.Command.SilenceUsage = true
	}

	i.setInput(args)

	// _lifecycle install
	if i.Input.NewLifecycle {
		return i.install()
	}

	// legacy LSCC install
	return i.installLegacy()
}

func (i *Installer) setInput(args []string) {
	i.Input = &InstallInput{
		Name:         chaincodeName,
		Version:      chaincodeVersion,
		Path:         chaincodePath,
		NewLifecycle: newLifecycle,
	}

	if len(args) > 0 {
		i.Input.PackageFile = args[0]
	}
}

// install installs a chaincode for use with _lifecycle
func (i *Installer) install() error {
	err := i.validateInput()
	if err != nil {
		return err
	}

	pkgBytes, err := i.Reader.ReadFile(i.Input.PackageFile)
	if err != nil {
		return errors.WithMessage(err, fmt.Sprintf("error reading chaincode package at %s", i.Input.PackageFile))
	}

	serializedSigner, err := i.Signer.Serialize()
	if err != nil {
		errors.WithMessage(err, fmt.Sprintf("error serializing signer for %v", i.Signer.GetIdentifier()))
	}

	proposal, err := i.createNewLifecycleInstallProposal(i.Input.Name, i.Input.Version, pkgBytes, serializedSigner)
	if err != nil {
		return err
	}

	signedProposal, err := protoutil.GetSignedProposal(proposal, i.Signer)
	if err != nil {
		return errors.WithMessage(err, fmt.Sprintf("error creating signed proposal for %s", chainFuncName))
	}

	return i.submitInstallProposal(signedProposal)
}

// installLegacy installs a chaincode deployment spec to "peer.address"
// for use with the legacy lscc
func (i *Installer) installLegacy() error {
	ccPkgMsg, err := i.getLegacyChaincodePackageMessage()
	if err != nil {
		return err
	}

	proposal, err := i.createLegacyInstallProposal(ccPkgMsg)
	if err != nil {
		return err
	}

	signedProposal, err := protoutil.GetSignedProposal(proposal, i.Signer)
	if err != nil {
		return errors.WithMessage(err, fmt.Sprintf("error creating signed proposal for %s", chainFuncName))
	}

	return i.submitInstallProposal(signedProposal)
}

func (i *Installer) submitInstallProposal(signedProposal *pb.SignedProposal) error {
	// install is currently only supported for one peer
	proposalResponse, err := i.EndorserClients[0].ProcessProposal(context.Background(), signedProposal)
	if err != nil {
		return errors.WithMessage(err, "error endorsing chaincode install")
	}

	if proposalResponse == nil {
		return errors.New("error during install: received nil proposal response")
	}

	if proposalResponse.Response == nil {
		return errors.New("error during install: received proposal response with nil response")
	}

	if proposalResponse.Response.Status != int32(cb.Status_SUCCESS) {
		return errors.Errorf("install failed with status: %d - %s", proposalResponse.Response.Status, proposalResponse.Response.Message)
	}
	logger.Infof("Installed remotely: %v", proposalResponse)

	if i.Input.NewLifecycle {
		icr := &lb.InstallChaincodeResult{}
		err := proto.Unmarshal(proposalResponse.Response.Payload, icr)
		if err != nil {
			return errors.Wrap(err, "error unmarshaling proposal response's response payload")
		}
		logger.Infof("Chaincode code package hash: %x", icr.Hash)
	}

	return nil
}

func (i *Installer) validateInput() error {
	if i.Input.PackageFile == "" {
		return errors.New("chaincode install package must be provided")
	}

	if i.Input.Name == "" {
		return errors.New("chaincode name must be specified")
	}

	if i.Input.Version == "" {
		return errors.New("chaincode version must be specified")
	}

	if i.Input.Path != "" {
		return errors.New("chaincode path parameter not supported by _lifecycle")
	}

	return nil
}

func (i *Installer) createNewLifecycleInstallProposal(name, version string, pkgBytes []byte, creatorBytes []byte) (*pb.Proposal, error) {
	installChaincodeArgs := &lb.InstallChaincodeArgs{
		Name:                    name,
		Version:                 version,
		ChaincodeInstallPackage: pkgBytes,
	}

	installChaincodeArgsBytes, err := proto.Marshal(installChaincodeArgs)
	if err != nil {
		return nil, errors.Wrap(err, "error marshaling InstallChaincodeArgs")
	}

	ccInput := &pb.ChaincodeInput{Args: [][]byte{[]byte("InstallChaincode"), installChaincodeArgsBytes}}

	cis := &pb.ChaincodeInvocationSpec{
		ChaincodeSpec: &pb.ChaincodeSpec{
			ChaincodeId: &pb.ChaincodeID{Name: newLifecycleName},
			Input:       ccInput,
		},
	}

	proposal, _, err := protoutil.CreateProposalFromCIS(cb.HeaderType_ENDORSER_TRANSACTION, "", cis, creatorBytes)
	if err != nil {
		return nil, errors.WithMessage(err, "error creating proposal for ChaincodeInvocationSpec")
	}

	return proposal, nil
}

func (i *Installer) getLegacyChaincodePackageMessage() (proto.Message, error) {
	// if no package provided, create one
	if i.Input.PackageFile == "" {
		if i.Input.Path == common.UndefinedParamValue || i.Input.Version == common.UndefinedParamValue || i.Input.Name == common.UndefinedParamValue {
			return nil, errors.Errorf("must supply value for %s name, path and version parameters", chainFuncName)
		}
		// generate a raw ChaincodeDeploymentSpec
		ccPkgMsg, err := genChaincodeDeploymentSpec(i.Command, i.Input.Name, i.Input.Version)
		if err != nil {
			return nil, err
		}
		return ccPkgMsg, nil
	}

	// read in a package generated by the "package" sub-command (and perhaps signed
	// by multiple owners with the "signpackage" sub-command)
	// var cds *pb.ChaincodeDeploymentSpec
	ccPkgMsg, cds, err := getPackageFromFile(i.Input.PackageFile)
	if err != nil {
		return nil, err
	}

	// get the chaincode details from cds
	cName := cds.ChaincodeSpec.ChaincodeId.Name
	cVersion := cds.ChaincodeSpec.ChaincodeId.Version

	// if user provided chaincodeName, use it for validation
	if i.Input.Name != "" && i.Input.Name != cName {
		return nil, errors.Errorf("chaincode name %s does not match name %s in package", i.Input.Name, cName)
	}

	// if user provided chaincodeVersion, use it for validation
	if i.Input.Version != "" && i.Input.Version != cVersion {
		return nil, errors.Errorf("chaincode version %s does not match version %s in packages", i.Input.Version, cVersion)
	}

	return ccPkgMsg, nil
}

func (i *Installer) createLegacyInstallProposal(msg proto.Message) (*pb.Proposal, error) {
	creator, err := i.Signer.Serialize()
	if err != nil {
		return nil, errors.WithMessage(err, fmt.Sprintf("error serializing identity for %s", i.Signer.GetIdentifier()))
	}

	prop, _, err := protoutil.CreateInstallProposalFromCDS(msg, creator)
	if err != nil {
		return nil, errors.WithMessage(err, fmt.Sprintf("error creating proposal for %s", chainFuncName))
	}

	return prop, nil
}

// genChaincodeDeploymentSpec creates ChaincodeDeploymentSpec as the package to install
func genChaincodeDeploymentSpec(cmd *cobra.Command, chaincodeName, chaincodeVersion string) (*pb.ChaincodeDeploymentSpec, error) {
	if existed, _ := ccprovider.ChaincodePackageExists(chaincodeName, chaincodeVersion); existed {
		return nil, errors.Errorf("chaincode %s:%s already exists", chaincodeName, chaincodeVersion)
	}

	spec, err := getChaincodeSpec(cmd)
	if err != nil {
		return nil, err
	}

	cds, err := getChaincodeDeploymentSpec(spec, true)
	if err != nil {
		return nil, errors.WithMessage(err, fmt.Sprintf("error getting chaincode deployment spec for %s", chaincodeName))
	}

	return cds, nil
}

// getPackageFromFile get the chaincode package from file and the extracted ChaincodeDeploymentSpec
func getPackageFromFile(ccPkgFile string) (proto.Message, *pb.ChaincodeDeploymentSpec, error) {
	ccPkgBytes, err := ioutil.ReadFile(ccPkgFile)
	if err != nil {
		return nil, nil, err
	}

	// the bytes should be a valid package (CDS or SignedCDS)
	ccpack, err := ccprovider.GetCCPackage(ccPkgBytes)
	if err != nil {
		return nil, nil, err
	}

	// either CDS or Envelope
	o := ccpack.GetPackageObject()

	// try CDS first
	cds, ok := o.(*pb.ChaincodeDeploymentSpec)
	if !ok || cds == nil {
		// try Envelope next
		env, ok := o.(*cb.Envelope)
		if !ok || env == nil {
			return nil, nil, errors.New("error extracting valid chaincode package")
		}

		// this will check for a valid package Envelope
		_, sCDS, err := ccpackage.ExtractSignedCCDepSpec(env)
		if err != nil {
			return nil, nil, errors.WithMessage(err, "error extracting valid signed chaincode package")
		}

		// ...and get the CDS at last
		cds, err = protoutil.GetChaincodeDeploymentSpec(sCDS.ChaincodeDeploymentSpec, platformRegistry)
		if err != nil {
			return nil, nil, errors.WithMessage(err, "error extracting chaincode deployment spec")
		}
	}

	return o, cds, nil
}
