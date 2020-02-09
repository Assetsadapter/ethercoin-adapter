/*
 * Copyright 2018 The openwallet Authors
 * This file is part of the openwallet library.
 *
 * The openwallet library is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Lesser General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * The openwallet library is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
 * GNU Lesser General Public License for more details.
 */
package ethercoin

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"strconv"
	"sync"

	//	"log"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/blocktree/go-owcrypt"
	"github.com/blocktree/openwallet/common"
	"github.com/blocktree/openwallet/common/file"
	"github.com/blocktree/openwallet/hdkeystore"
	"github.com/blocktree/openwallet/log"
	"github.com/blocktree/openwallet/openwallet"
)

const (
	maxAddresNum = 1000000
)

const (
	WALLET_NOT_EXIST_ERR        = "The wallet whose name is given not exist!"
	BACKUP_FILE_TYPE_ADDRESS    = 0
	BACKUP_FILE_TYPE_WALLET_KEY = 1
	BACKUP_FILE_TYPE_WALLET_DB  = 2
)

var (
// 节点客户端
//	client *Client

//秘钥存取
//storage *keystore.HDKeystore
//全局的manager
//	g_manager *WalletManager
)

/*func init() {
	storage = keystore.NewHDKeystore(KeyDir, keystore.StandardScryptN, keystore.StandardScryptP)
	client = &Client{BaseURL: serverAPI, Debug: true}
}*/

type WalletManager struct {
	openwallet.AssetsAdapterBase

	Storage      *hdkeystore.HDKeystore        //秘钥存取
	WalletClient *Client                       // 节点客户端
	Config       *WalletConfig                 //钱包管理配置
	WalletsInSum map[string]*openwallet.Wallet //参与汇总的钱包
	Blockscanner openwallet.BlockScanner       //区块扫描器
	Decoder      openwallet.AddressDecoder     //地址编码器
	TxDecoder    openwallet.TransactionDecoder //交易单编码器
	//	RootDir        string                        //
	locker          sync.Mutex //防止并发修改和读取配置, 可能用不上
	WalletInSumOld  map[string]*Wallet
	ContractDecoder openwallet.SmartContractDecoder //
	//StorageOld      *keystore.HDKeystore
	ConfigPath    string
	RootPath      string
	DefaultConfig string
	//SymbolID        string

	Log *log.OWLogger //日志工具
}

func (this *WalletManager) GetConfig() WalletConfig {
	this.locker.Lock()
	defer this.locker.Unlock()
	return *this.Config
}

func NewWalletManager() *WalletManager {
	wm := WalletManager{}
	//wm.RootPath = "data"
	//wm.ConfigPath = "conf"
	//wm.SymbolID = Symbol
	wm.Config = NewConfig(Symbol)
	//wm.Config = &WalletConfig{}
	//wm.DefaultConfig = makeEthDefaultConfig(wm.ConfigPath)

	//参与汇总的钱包
	//wm.WalletsInSum = make(map[string]*openwallet.Wallet)
	//区块扫描器
	wm.Blockscanner = NewETEBlockScanner(&wm)
	wm.Decoder = &AddressDecoder{}
	wm.TxDecoder = NewTransactionDecoder(&wm)

	//wm.NewConfig(wm.RootPath, MasterKey)

	//wm.StorageOld = keystore.NewHDKeystore(wm.Config.KeyDir, keystore.StandardScryptN, keystore.StandardScryptP)
	//storage := hdkeystore.NewHDKeystore(wm.Config.KeyDir, hdkeystore.StandardScryptN, hdkeystore.StandardScryptP)
	//wm.Storage = storage
	client := &Client{BaseURL: wm.Config.ServerAPI, Debug: false}
	wm.WalletClient = client
	wm.ContractDecoder = &EthContractDecoder{
		wm: &wm,
	}

	//wm.WalletInSumOld = make(map[string]*Wallet)
	wm.Log = log.NewOWLogger(wm.Symbol())

	return &wm
}

func (this *WalletManager) CreateWallet(name string, password string) (*Wallet, string, error) {
	//检查钱包名是否存在
	wallets, err := GetWalletKeys(this.GetConfig().KeyDir)
	if err != nil {
		log.Error("GetWalletKeys failed, err=", err)
		return nil, "", errors.New(fmt.Sprintf("get wallet keys failed, err = %v", err))
	}

	for _, w := range wallets {
		if w.Alias == name {
			log.Error("duplicated alias.")
			return nil, "", errors.New("The wallet's alias is duplicated!")
		}
	}

	//生成keystore
	key, filePath, err := hdkeystore.StoreHDKey(this.GetConfig().KeyDir, name, password, hdkeystore.StandardScryptN, hdkeystore.StandardScryptP)
	if err != nil {
		log.Error("create hdkeystore failed, err=", err)
		return nil, "", err
	}

	// root/n' , 使用强化方案
	hdPath := fmt.Sprintf("%s/%d'", key.RootPath, 1)
	childKey, err := key.DerivedKeyWithPath(hdPath, owcrypt.ECC_CURVE_SECP256K1)
	if err != nil {
		log.Error("generate child key failed, err=", err)
		return nil, "", err
	}

	publicKey := childKey.GetPublicKey().OWEncode()

	w := Wallet{
		WalletID:  key.KeyID,
		Alias:     key.Alias,
		RootPath:  key.RootPath,
		KeyFile:   filePath,
		HdPath:    hdPath,
		PublicKey: publicKey,
	}

	db, err := w.OpenDB(this.GetConfig().DbPath)
	if err != nil {
		log.Error("open wallet db[", w.Alias, "] failed, err=")
		return nil, "", err
	}
	defer db.Close()

	err = db.Save(&w)
	if err != nil {
		log.Error("save wallet[", w.Alias, "] to db failed, err=", err)
		return nil, "", err
	}

	return &w, filePath, nil
}

func GetWalletKey(fileWitoutProfix string) (*Wallet, error) {

	keyfile := fileWitoutProfix + ".key"
	//dbfile := fileWitoutProfix + ".db"
	finfo, err := os.Stat(keyfile)
	if err != nil {
		log.Errorf("stat file [%v] failed, err = %v", keyfile, err)
		return nil, err
	}

	/*if strings.Index(finfo.Name(), ".key") != (len(finfo.Name()) - 5) {
		this.Log.Errorf("file name error")
		return nil, errors.New("verify key file name error")
	}*/
	var key struct {
		Alias  string `json:"alias"`
		RootId string `json:"rootid"`
	}
	buf := new(bufio.Reader)

	fd, err := os.Open(keyfile)
	defer fd.Close()
	if err != nil {
		log.Errorf("get wallet key, open db failed, err = %v", err)
		return nil, err
	}

	buf.Reset(fd)
	// Parse the address.
	key.Alias = ""
	key.RootId = ""
	err = json.NewDecoder(buf).Decode(&key)
	if err != nil {
		log.Errorf("decode key file error, err = %v", err)
		return nil, err
	}

	return &Wallet{WalletID: key.RootId, Alias: key.Alias, KeyFile: finfo.Name()}, nil
}

func GetWalletKeys(dir string) ([]*Wallet, error) {
	type keyType struct {
		Alias    string `json:"alias"`
		KeyId    string `json:"keyid"`
		RootPath string `json:"rootpath"`
	}
	buf := new(bufio.Reader)
	wallets := make([]*Wallet, 0)

	//加载文件，实例化钱包
	readWallet := func(path string) *Wallet {

		fd, err := os.Open(path)
		defer fd.Close()
		if err != nil {
			return nil
		}

		buf.Reset(fd)
		// Parse the address.
		var key keyType
		err = json.NewDecoder(buf).Decode(&key)
		if err != nil {
			return nil
		}

		return &Wallet{WalletID: key.KeyId, Alias: key.Alias, RootPath: key.RootPath}
	}

	//扫描key目录的所有钱包
	absPath, _ := filepath.Abs(dir)
	file.MkdirAll(absPath)
	files, err := ioutil.ReadDir(absPath)
	if err != nil {
		return wallets, err
	}

	for _, fi := range files {
		// Skip any non-key files from the folder
		if skipKeyFile(fi) {
			continue
		}
		if fi.IsDir() {
			continue
		}

		path := filepath.Join(dir, fi.Name())

		w := readWallet(path)
		w.KeyFile = fi.Name()
		//fmt.Println("absolute path:", absPath)
		wallets = append(wallets, w)

	}

	return wallets, nil
}

func skipKeyFile(fi os.FileInfo) bool {
	// Skip editor backups and UNIX-style hidden files.
	if strings.HasSuffix(fi.Name(), "~") || strings.HasPrefix(fi.Name(), ".") {
		return true
	}
	// Skip misc special files, directories (yes, symlinks too).
	if fi.IsDir() || fi.Mode()&os.ModeType != 0 {
		return true
	}

	return false
}

func (this *WalletManager) SaveERC20TokenConfig(config *ERC20Token) error {
	db, err := OpenDB(this.GetConfig().DbPath, ERC20TOKEN_DB)
	defer db.Close()
	if err != nil {
		this.Log.Errorf("open db for path [%v] failed, err = %v", this.GetConfig().DbPath+"/"+ERC20TOKEN_DB, err)
		return err
	}
	err = db.Save(config)
	if err != nil {
		this.Log.Errorf("save db for path [%v] failed, err = %v", this.GetConfig().DbPath+"/"+ERC20TOKEN_DB, err)
		return err
	}
	return nil
}

func (this *WalletManager) GetERC20TokenList() ([]ERC20Token, error) {
	db, err := OpenDB(this.GetConfig().DbPath, ERC20TOKEN_DB)
	defer db.Close()
	if err != nil {
		this.Log.Errorf("open db for path [%v] failed, err = %v", this.GetConfig().DbPath+"/"+ERC20TOKEN_DB, err)
		return nil, err
	}
	tokens := make([]ERC20Token, 0)
	err = db.All(&tokens)
	if err != nil {
		this.Log.Errorf("query token list in db failed, err= %v", err)
		return nil, err
	}
	return tokens, nil
}

//AddWalletInSummary 添加汇总钱包账户
func (this *WalletManager) AddWalletInSummary(wid string, wallet *Wallet) {
	this.WalletInSumOld[wid] = wallet
}

//阻塞式的执行外部shell命令的函数,等待执行完毕并返回标准输出
func exec_shell(s string) (string, error) {
	//函数返回一个*Cmd，用于使用给出的参数执行name指定的程序
	cmd := exec.Command("/bin/bash", "-c", s)

	//读取io.Writer类型的cmd.Stdout，再通过bytes.Buffer(缓冲byte类型的缓冲器)将byte类型转化为string类型(out.String():这是bytes类型提供的接口)
	var out bytes.Buffer
	cmd.Stdout = &out

	//Run执行c包含的命令，并阻塞直到完成。  这里stdout被取出，cmd.Wait()无法正确获取stdin,stdout,stderr，则阻塞在那了
	err := cmd.Run()

	return out.String(), err
}

func (this *WalletManager) BackupWalletToDefaultPath(wallet *Wallet, password string) (string, error) {
	newBackupDir := filepath.Join(this.GetConfig().BackupDir, wallet.FileName()+"-"+common.TimeFormat(TIME_POSTFIX))
	return this.BackupWallet2(newBackupDir, wallet, password)
}

func (this *WalletManager) BackupWallet2(newBackupDir string, wallet *Wallet,
	password string) (string, error) {
	err := this.UnlockWallet(wallet, password)
	if err != nil {
		this.Log.Errorf("unlock wallet failed, err=%v", err)
		return "", err
	}

	keyFile := filepath.Join(this.GetConfig().KeyDir, wallet.FileName()+".key")
	dbFile := filepath.Join(this.GetConfig().DbPath, wallet.FileName()+".db")

	file.MkdirAll(newBackupDir)

	//备份钱包key文件
	err = file.Copy(keyFile, newBackupDir)
	if err != nil {
		log.Error("backup key file [", keyFile, "] to ", newBackupDir, " failed, err=", err)
		return "", err
	}

	//备份地址数据库
	err = file.Copy(dbFile, newBackupDir)
	if err != nil {
		log.Error("backup db file [", dbFile, "] to ", newBackupDir, " failed, err=", err)
		return "", err
	}
	return newBackupDir, nil
}

func (this *WalletManager) UnlockWallet(wallet *Wallet, password string) error {
	_, err := wallet.HDKey2(password)
	if err != nil {
		this.Log.Errorf(fmt.Sprintf("get HDkey, err=%v\n", err))
		return err
	}
	return nil
}

//exportAddressToFile 导出地址到文件中
func (this *WalletManager) exportAddressToFile(addrs []*Address, filePath string) error {

	var content string

	for _, a := range addrs {
		//log.Std.Info("Export: %s ", a.Address)
		content = content + AppendOxToAddress(a.Address) + "\n"
	}

	file.MkdirAll(this.GetConfig().AddressDir)
	if !file.WriteFile(filePath, []byte(content), true) {
		return errors.New("export address to file failed.")
	}
	return nil
}

type AddrVec struct {
	addrs []*Address
}

func (this *AddrVec) Len() int {
	return len(this.addrs)
}

func (this *AddrVec) Swap(i, j int) {
	this.addrs[i], this.addrs[j] = this.addrs[j], this.addrs[i]
}

func (this *AddrVec) Less(i, j int) bool {
	if this.addrs[i].balance.Cmp(this.addrs[j].balance) < 0 {
		return true
	}
	return false
}

type TokenAddrVec struct {
	addrs []*Address
}

func (this *TokenAddrVec) Len() int {
	return len(this.addrs)
}

func (this *TokenAddrVec) Swap(i, j int) {
	this.addrs[i], this.addrs[j] = this.addrs[j], this.addrs[i]
}

func (this *TokenAddrVec) Less(i, j int) bool {
	if this.addrs[i].tokenBalance.Cmp(this.addrs[j].tokenBalance) < 0 {
		return true
	}
	return false
}

type txFeeInfo struct {
	GasLimit *big.Int
	GasPrice *big.Int
	Fee      *big.Int
}

func (this *txFeeInfo) CalcFee() error {
	fee := new(big.Int)
	fee.Mul(this.GasLimit, this.GasPrice)
	this.Fee = fee
	return nil
}

func (this *WalletManager) GetTransactionFeeEstimated(from string, to string, value *big.Int, data string) (*txFeeInfo, error) {

	var (
		gasLimit *big.Int
		err      error
	)
	if this.Config.FixGasLimit.Cmp(big.NewInt(0)) > 0 {
		//配置设置固定gasLimit
		gasLimit = this.Config.FixGasLimit
	} else {
		//动态计算gas消耗
		gasLimit, err = this.WalletClient.ethGetGasEstimated(makeGasEstimatePara(from, to, value, data))
		if err != nil {
			this.Log.Errorf(fmt.Sprintf("get gas limit failed, err = %v\n", err))
			return nil, err
		}
	}

	gasPrice, err := this.WalletClient.ethGetGasPrice()
	if err != nil {
		this.Log.Errorf(fmt.Sprintf("get gas price failed, err = %v\n", err))
		return nil, err
	}

	//	fee := new(big.Int)
	//	fee.Mul(gasLimit, gasPrice)

	feeInfo := &txFeeInfo{
		GasLimit: gasLimit,
		GasPrice: gasPrice,
		//		Fee:      fee,
	}

	feeInfo.CalcFee()
	return feeInfo, nil
}

func (this *WalletManager) GetERC20TokenTransactionFeeEstimated(from string, to string, data string) (*txFeeInfo, error) {
	/*gasLimit, err := ethGetGasEstimated(makeERC20TokenTransGasEstimatePara(from, to, data))
	if err != nil {
		this.Log.Errorf(fmt.Sprintf("get gas limit failed, err = %v\n", err))
		return nil, err
	}

	gasPrice, err := ethGetGasPrice()
	if err != nil {
		this.Log.Errorf(fmt.Sprintf("get gas price failed, err = %v\n", err))
		return nil, err
	}

	fee := new(big.Int)
	fee.Mul(gasLimit, gasPrice)

	feeInfo := &txFeeInfo{
		GasLimit: gasLimit,
		GasPrice: gasPrice,
		Fee:      fee,
	}
	return feeInfo, nil*/
	return this.GetTransactionFeeEstimated(from, to, nil, data)
}

func (this *WalletManager) GetSimpleTransactionFeeEstimated(from string, to string, amount *big.Int) (*txFeeInfo, error) {
	/*gasLimit, err := ethGetGasEstimated(makeSimpleTransGasEstimatedPara(from, to, amount))
	if err != nil {
		this.Log.Errorf(fmt.Sprintf("get gas limit failed, err = %v\n", err))
		return nil, err
	}

	gasPrice, err := ethGetGasPrice()
	if err != nil {
		this.Log.Errorf(fmt.Sprintf("get gas price failed, err = %v\n", err))
		return nil, err
	}

	fee := new(big.Int)
	fee.Mul(gasLimit, gasPrice)

	feeInfo := &txFeeInfo{
		GasLimit: gasLimit,
		GasPrice: gasPrice,
		Fee:      fee,
	}
	return feeInfo, nil*/
	return this.GetTransactionFeeEstimated(from, to, amount, "")
}

func removeOxFromHex(value string) string {
	result := value
	if strings.Index(value, "0x") != -1 {
		result = common.Substr(value, 2, len(value))
	}
	return result
}

func ConvertToUint64(value string, base int) (uint64, error) {
	v := value
	if base == 16 {
		v = removeOxFromHex(v)
	}

	rst, err := strconv.ParseUint(v, base, 64)
	if err != nil {
		log.Errorf("convert string[%v] to int failed, err = %v", value, err)
		return 0, err
	}
	return rst, nil
}

func ConvertToBigInt(value string, base int) (*big.Int, error) {
	bigvalue := new(big.Int)
	var success bool
	if base == 16 {
		value = removeOxFromHex(value)
	}

	if value == "" {
		value = "0"
	}

	_, success = bigvalue.SetString(value, base)
	if !success {
		errInfo := fmt.Sprintf("convert value [%v] to bigint failed, check the value and base passed through\n", value)
		log.Errorf(errInfo)
		return big.NewInt(0), errors.New(errInfo)
	}
	return bigvalue, nil
}

func (this *Client) UnlockAddr(address string, password string, secs int) error {
	params := []interface{}{
		address,
		password,
		secs,
	}

	result, err := this.Call("personal_unlockAccount", 1, params)
	if err != nil {
		log.Errorf(fmt.Sprintf("unlock address [%v] faield, err = %v \n", address, err))
		return err
	}

	if result.Type != gjson.True {
		log.Errorf(fmt.Sprintf("unlock address [%v] failed", address))
		return errors.New("unlock failed")
	}

	return nil
}

func (this *Client) LockAddr(address string) error {
	params := []interface{}{
		address,
	}

	result, err := this.Call("personal_lockAccount", 1, params)
	if err != nil {
		errInfo := fmt.Sprintf("lock address [%v] faield, err = %v \n", address, err)
		log.Errorf(errInfo)
		return err
	}

	if result.Type != gjson.True {
		errInfo := fmt.Sprintf("lock address [%v] failed", address)
		log.Errorf(errInfo)
		return errors.New(errInfo)
	}

	return nil
}

/*func createRawTransaction(from string, to string, value *big.Int, data string) ([]byte, error) {
	fee, err := GetTransactionFeeEstimated(from, to, value, data)
	if err != nil {
		this.Log.Errorf("GetTransactionFeeEstimated from[%v] -> to[%v] failed, err=%v", from, to, err)
		return nil, err
	}

	nonce, err := GetNonceForAddress2(from)
	if err != nil {
		this.Log.Errorf("GetNonceForAddress from[%v] failed, err=%v", from, err)
		return nil, err
	}

	signer := types.NewEIP155Signer(big.NewInt(CHAIN_ID))

	tx := types.NewTransaction(nonce, ethcommon.HexToAddress(to),
		value, fee.GasLimit.Uint64(), fee.GasPrice, []byte(data))
	msg := signer.Hash(tx)
	return msg[:], nil
}*/

func verifyTransaction(nonce uint64, to string, amount *big.Int, gasLimit uint64, gasPrice *big.Int, data []byte) error {
	return nil
}

func (this *Client) ethGetGasPrice() (*big.Int, error) {
	params := []interface{}{}
	result, err := this.Call("eth_gasPrice", 1, params)
	if err != nil {
		log.Errorf(fmt.Sprintf("get gas price failed, err = %v \n", err))
		return big.NewInt(0), err
	}

	if result.Type != gjson.String {
		log.Errorf(fmt.Sprintf("get gas price failed, response is %v\n", err))
		return big.NewInt(0), err
	}

	gasLimit, err := ConvertToBigInt(result.String(), 16)
	if err != nil {
		errInfo := fmt.Sprintf("convert estimated gas[%v] format to bigint failed, err = %v\n", result.String(), err)
		log.Errorf(errInfo)
		return big.NewInt(0), errors.New(errInfo)
	}
	return gasLimit, nil
}

func (this *WalletManager) GetNonceForAddress2(address string) (uint64, error) {
	address = AppendOxToAddress(address)
	//txpool, err := this.WalletClient.EthGetTxPoolContent()
	//if err != nil {
	//	this.Log.Errorf("EthGetTxPoolContent failed, err = %v", err)
	//	return 0, err
	//}
	//
	//_, max, count, err := txpool.GetSequentTxNonce(address)
	//if err != nil {
	//	log.Error("get txpool sequent tx nonce failed, err=%v", err)
	//	return 0, err
	//}
	//log.Debugf("sequent max nonce:%v", max)
	//log.Debugf("sequent nonce count:%v", count)
	txCount, err := this.WalletClient.ethGetTransactionCount(address)
	if err != nil {
		log.Error("ethGetTransactionCount failed, err=", err)
		return 0, err
	}
	log.Debugf("txCount:%v", txCount)
	return txCount, nil
	//if count == 0 || max < txCount {
	//	return txCount, nil
	//}
	//return max + 1, nil
}

func (this *WalletManager) GetNonceForAddress(address string) (uint64, error) {
	txpool, err := this.WalletClient.EthGetTxPoolContent()
	if err != nil {
		this.Log.Errorf("EthGetTxPoolContent failed, err = %v", err)
		return 0, err
	}

	txCount := txpool.GetPendingTxCountForAddr(address)
	this.Log.Infof("address[%v] has %v tx in pending queue of txpool.", address, txCount)
	for txCount > 0 {
		time.Sleep(time.Second * 1)
		txpool, err = this.WalletClient.EthGetTxPoolContent()
		if err != nil {
			this.Log.Errorf("EthGetTxPoolContent failed, err = %v", err)
			return 0, err
		}

		txCount = txpool.GetPendingTxCountForAddr(address)
		this.Log.Infof("address[%v] has %v tx in pending queue of txpool.", address, txCount)
	}

	nonce, err := this.WalletClient.ethGetTransactionCount(address)
	if err != nil {
		this.Log.Errorf("ethGetTransactionCount failed, err=%v", err)
		return 0, err
	}
	return nonce, nil
}
