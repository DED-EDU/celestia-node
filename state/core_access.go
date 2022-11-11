package state

import (
    "context"
    "errors"
    "fmt"
    "time"
	"io"
	"io/fs"
	"os"

    "github.com/cosmos/cosmos-sdk/api/tendermint/abci"
    "github.com/cosmos/cosmos-sdk/store/rootmulti"
    sdktypes "github.com/cosmos/cosmos-sdk/types"
    sdktx "github.com/cosmos/cosmos-sdk/types/tx"
    banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
    stakingtypes "github.com/cosmos/cosmos-sdk/x/staking/types"
    logging "github.com/ipfs/go-log/v2"
    rpcclient "github.com/tendermint/tendermint/rpc/client"
    "github.com/tendermint/tendermint/rpc/client/http"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"

    "github.com/celestiaorg/celestia-app/app"
    "github.com/celestiaorg/celestia-app/x/payment"
    apptypes "github.com/celestiaorg/celestia-app/x/payment/types"
    "github.com/celestiaorg/nmt/namespace"

    "github.com/celestiaorg/celestia-node/header"

	"github.com/ipfs/go-cid"
	"github.com/web3-storage/go-w3s-client"
	w3fs "github.com/web3-storage/go-w3s-client/fs"
)

// Usage:
// TOKEN="API_TOKEN" go run ./main.go
func main() {
	c, err := w3s.NewClient(
		w3s.WithEndpoint(os.Getenv("ENDPOINT")),
		w3s.WithToken(os.Getenv("TOKEN")),
	)
	if err != nil {
		panic(err)
	}

	// cid := putSingleFile(c)
	// getStatusForCid(c, cid)
	// getStatusForKnownCid(c)
	getFiles(c)
	// listUploads(c)
}

var (
    log              = logging.Logger("state")
    ErrInvalidAmount = errors.New("state: amount must be greater than zero")
)

// CoreAccessor implements service over a gRPC connection
// with a celestia-core node.
type CoreAccessor struct {
    ctx    context.Context
    cancel context.CancelFunc

    signer *apptypes.KeyringSigner
    getter header.Head

    queryCli   banktypes.QueryClient
    stakingCli stakingtypes.QueryClient
    rpcCli     rpcclient.ABCIClient

    coreConn *grpc.ClientConn
    coreIP   string
    rpcPort  string
    grpcPort string

    lastPayForData  int64
    payForDataCount int64
}

// NewCoreAccessor dials the given celestia-core endpoint and
// constructs and returns a new CoreAccessor (state service) with the active
// connection.
func NewCoreAccessor(
    signer *apptypes.KeyringSigner,
    getter header.Head,
    coreIP,
    rpcPort string,
    grpcPort string,
) *CoreAccessor {
    return &CoreAccessor{
        signer:   signer,
        getter:   getter,
        coreIP:   coreIP,
        rpcPort:  rpcPort,
        grpcPort: grpcPort,
    }
}

func (ca *CoreAccessor) Start(ctx context.Context) error {
    if ca.coreConn != nil {
        return fmt.Errorf("core-access: already connected to core endpoint")
    }
    ca.ctx, ca.cancel = context.WithCancel(context.Background())

    // dial given celestia-core endpoint
    endpoint := fmt.Sprintf("%s:%s", ca.coreIP, ca.grpcPort)
    client, err := grpc.DialContext(ctx, endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
    if err != nil {
        return err
    }
    ca.coreConn = client
    // create the query client
    queryCli := banktypes.NewQueryClient(ca.coreConn)
    ca.queryCli = queryCli
    // create the staking query client
    stakingCli := stakingtypes.NewQueryClient(ca.coreConn)
    ca.stakingCli = stakingCli
    // create ABCI query client
    cli, err := http.New(fmt.Sprintf("http://%s:%s", ca.coreIP, ca.rpcPort), "/websocket")
    if err != nil {
        return err
    }
    ca.rpcCli = cli
    return nil
}

func (ca *CoreAccessor) Stop(context.Context) error {
    if ca.cancel == nil {
        log.Warn("core accessor already stopped")
        return nil
    }
    if ca.coreConn == nil {
        log.Warn("no connection found to close")
        return nil
    }
    defer ca.cancelCtx()

    // close out core connection
    err := ca.coreConn.Close()
    if err != nil {
        return err
    }

    ca.coreConn = nil
    ca.queryCli = nil
    return nil
}

func (ca *CoreAccessor) cancelCtx() {
    ca.cancel()
    ca.cancel = nil
}

func (ca *CoreAccessor) constructSignedTx(
    ctx context.Context,
    msg sdktypes.Msg,
    opts ...apptypes.TxBuilderOption,
) ([]byte, error) {
    // should be called first in order to make a valid tx
    err := ca.signer.QueryAccountNumber(ctx, ca.coreConn)
    if err != nil {
        return nil, err
    }

    tx, err := ca.signer.BuildSignedTx(ca.signer.NewTxBuilder(opts...), msg)
    if err != nil {
        return nil, err
    }
    return ca.signer.EncodeTx(tx)
}

func (ca *CoreAccessor) SubmitPayForData(
    ctx context.Context,
    nID namespace.ID,
    data []byte,
    gasLim uint64,
) (*TxResponse, error) {
    response, err := payment.SubmitPayForData(ctx, ca.signer, ca.coreConn, nID, data, gasLim)
    // metrics should only be counted on a successful PFD tx
    if response.Code == 0 && err == nil {
        ca.lastPayForData = time.Now().UnixMilli()
        ca.payForDataCount++
    }
    return response, err
}

func (ca *CoreAccessor) AccountAddress(ctx context.Context) (Address, error) {
    addr, err := ca.signer.GetSignerInfo().GetAddress()
    if err != nil {
        return nil, err
    }
    return addr, nil
}

func (ca *CoreAccessor) Balance(ctx context.Context) (*Balance, error) {
    addr, err := ca.signer.GetSignerInfo().GetAddress()
    if err != nil {
        return nil, err
    }
    return ca.BalanceForAddress(ctx, addr)
}

func (ca *CoreAccessor) BalanceForAddress(ctx context.Context, addr Address) (*Balance, error) {
    head, err := ca.getter.Head(ctx)
    if err != nil {
        return nil, err
    }
    // construct an ABCI query for the height at head-1 because
    // the AppHash contained in the head is actually the state root
    // after applying the transactions contained in the previous block.
    // TODO @renaynay: once https://github.com/cosmos/cosmos-sdk/pull/12674 is merged, use this method
    // instead
    prefixedAccountKey := append(banktypes.CreateAccountBalancesPrefix(addr.Bytes()), []byte(app.BondDenom)...)
    abciReq := abci.RequestQuery{
        // TODO @renayay: once https://github.com/cosmos/cosmos-sdk/pull/12674 is merged, use const instead
        Path:   fmt.Sprintf("store/%s/key", banktypes.StoreKey),
        Height: head.Height - 1,
        Data:   prefixedAccountKey,
        Prove:  true,
    }
    opts := rpcclient.ABCIQueryOptions{
        Height: abciReq.Height,
        Prove:  abciReq.Prove,
    }
    result, err := ca.rpcCli.ABCIQueryWithOptions(ctx, abciReq.Path, abciReq.Data, opts)
    if err != nil {
        return nil, err
    }
    if !result.Response.IsOK() {
        return nil, sdkErrorToGRPCError(result.Response)
    }
    // unmarshal balance information
    value := result.Response.Value
    // if the value returned is empty, the account balance does not yet exist
    if len(value) == 0 {
        log.Errorf("balance for account %s does not exist at block height %d", addr.String(), head.Height-1)
        return &Balance{
            Denom:  app.BondDenom,
            Amount: sdktypes.NewInt(0),
        }, nil
    }
    coin, ok := sdktypes.NewIntFromString(string(value))
    if !ok {
        return nil, fmt.Errorf("cannot convert %s into sdktypes.Int", string(value))
    }
    // verify balance
    path := fmt.Sprintf("/%s/%s", banktypes.StoreKey, string(prefixedAccountKey))
    prt := rootmulti.DefaultProofRuntime()
    err = prt.VerifyValue(result.Response.GetProofOps(), head.AppHash, path, value)
    if err != nil {
        return nil, err
    }

    return &Balance{
        Denom:  app.BondDenom,
        Amount: coin,
    }, nil
}

func (ca *CoreAccessor) SubmitTx(ctx context.Context, tx Tx) (*TxResponse, error) {
    txResp, err := apptypes.BroadcastTx(ctx, ca.coreConn, sdktx.BroadcastMode_BROADCAST_MODE_BLOCK, tx)
    if err != nil {
        return nil, err
    }
    return txResp.TxResponse, nil
}

// func putSingleFile(c w3s.Client) cid.Cid {
// 	file, err := os.Open("images/exampleq.jpg")
// 	if err != nil {
// 		panic(err)
// 	}
// 	return putFile(c, file)
// }

func putMultipleFiles(c w3s.Client) cid.Cid {
// 	f0, err := os.Open("images/eample.jpg")
// 	if err != nil {
// 		panic(err)
// 	}
// 	f1, err := os.Open("images/example.jpg")
// 	if err != nil {
// 		panic(err)
// 	}
	// dir := w3fs.NewDir("comic", []fs.File{f0, f1})
	// return putFile(c, dir)
}

// func putMultipleFilesAndDirectories(c w3s.Client) cid.Cid {
// 	f0, err := os.Open("images/examplezz.jpg")
// 	if err != nil {
// 		panic(err)
// 	}
// 	f1, err := os.Open("images/examples.jpg")
// 	if err != nil {
// 		panic(err)
// 	}
// 	d0 := w3fs.NewDir("one", []fs.File{f0})
// 	d1 := w3fs.NewDir("two", []fs.File{f1})
// 	rootdir := w3fs.NewDir("comic", []fs.File{d0, d1})
// 	return putFile(c, rootdir)
// }

// func putDirectory(c w3s.Client) cid.Cid {
// 	dir, err := os.Open("images")
// 	if err != nil {
// 		panic(err)
// 	}
// 	return putFile(c, dir)
// }

// func putFile(c w3s.Client, f fs.File, opts ...w3s.PutOption) cid.Cid {
// 	cid, err := c.Put(context.Background(), f, opts...)
// 	if err != nil {
// 		panic(err)
// 	}
// 	fmt.Printf("https://%v.ipfs.dweb.link\n", cid)
// 	return cid
// }


// write a hook that takes data and puts on Filecoin in function below
func (ca *CoreAccessor) SubmitTxWithBroadcastMode(
    ctx context.Context,
    tx Tx,
    mode sdktx.BroadcastMode,
) (*TxResponse, error) {
    txResp, err := apptypes.BroadcastTx(ctx, ca.coreConn, mode, tx)
    if err != nil {
        return nil, err
    }
    // first attempt a single (tx) file upload
    // cid := putSingleFile(ca.coreConn)
    
    // then attempt to upload multiple tx files
	// tx = w3fs.putFiles(c w3s.Client) cid.Cid {
    return txResp.TxResponse, nil
}

func putFile(c w3s.Client, f fs.File, opts ...w3s.PutOption) cid.Cid {
	// cid, err := c.Put(context.Background(), f, opts...)
    cid, err := c.Put(context.Background(), TxResponse, opts...)
	if err != nil {
		panic(err)
	}
	fmt.Printf("https://%v.ipfs.dweb.link\n", cid)
	return cid

// write a hook that takes data and puts on Filecoin in function below

// To use Web3.storage the user must have an API token. This token can be generated once an account is created: https://web3.storage/docs/intro/#get-an-api-token
// Ensure the proper submit PayForData is POST, with the body including a field for the file(s) uploaded to Filecoin (using Web3.storage)
// func (ca *CoreAccessor) SubmitData(ctx context.Context, data []byte) (*TxResponse, error) {
    // c, _ := w3s.NewClient(w3s.WithToken("<AUTH_TOKEN>"))
    // f, _ := os.Open("images/examples.jpg")       // create image file in aforementioned directory
    // // OR add a whole directory:
    // //
    // //   f, _ := os.Open("images")
    // //
    // // OR create your own directory:
    // //
    // //   img0, _ := os.Open("aliens.jpg")
    // //   img1, _ := os.Open("donotresist.jpg")
    // //   f := w3fs.NewDir("images", []fs.File{img0, img1})

    // // Write a file/directory
    // cid, _ := c.Put(context.Background(), f)
    // fmt.Printf("https://%v.ipfs.dweb.link\n", cid)

    // // Retrieve a file/directory
    // res, _ := c.Get(context.Background(), cid)

    // // res is a http.Response with an extra method for reading IPFS UnixFS files!
    // f, fsys, _ := res.Files()
    // return ca.SubmitPayForData(ctx, namespace.ID{}, data, 0)
// }

// write a hook that takes data and puts on Filecoin in function below

// func (ca *CoreAccessor) SubmitTxWithBroadcastMode(
//  ctx context.Context,
//  tx Tx,
//  mode sdktx.BroadcastMode,
// ) (*TxResponse, error) {
//  txResp, err := apptypes.BroadcastTx(ctx, ca.coreConn, mode, tx)
//  if err != nil {
//      return nil, err
//  }
//  return txResp.TxResponse, nil
// }

func (ca *CoreAccessor) Transfer(
    ctx context.Context,
    addr AccAddress,
    amount Int,
    gasLim uint64,
) (*TxResponse, error) {
    if amount.IsNil() || amount.Int64() <= 0 {
        return nil, ErrInvalidAmount
    }

    from, err := ca.signer.GetSignerInfo().GetAddress()
    if err != nil {
        return nil, err
    }
    coins := sdktypes.NewCoins(sdktypes.NewCoin(app.BondDenom, amount))
    msg := banktypes.NewMsgSend(from, addr, coins)
    signedTx, err := ca.constructSignedTx(ctx, msg, apptypes.SetGasLimit(gasLim))
    if err != nil {
        return nil, err
    }
    return ca.SubmitTx(ctx, signedTx)
}

func (ca *CoreAccessor) CancelUnbondingDelegation(
    ctx context.Context,
    valAddr ValAddress,
    amount,
    height Int,
    gasLim uint64,
) (*TxResponse, error) {
    if amount.IsNil() || amount.Int64() <= 0 {
        return nil, ErrInvalidAmount
    }

    from, err := ca.signer.GetSignerInfo().GetAddress()
    if err != nil {
        return nil, err
    }
    coins := sdktypes.NewCoin(app.BondDenom, amount)
    msg := stakingtypes.NewMsgCancelUnbondingDelegation(from, valAddr, height.Int64(), coins)
    signedTx, err := ca.constructSignedTx(ctx, msg, apptypes.SetGasLimit(gasLim))
    if err != nil {
        return nil, err
    }
    return ca.SubmitTx(ctx, signedTx)
}

func (ca *CoreAccessor) BeginRedelegate(
    ctx context.Context,
    srcValAddr,
    dstValAddr ValAddress,
    amount Int,
    gasLim uint64,
) (*TxResponse, error) {
    if amount.IsNil() || amount.Int64() <= 0 {
        return nil, ErrInvalidAmount
    }

    from, err := ca.signer.GetSignerInfo().GetAddress()
    if err != nil {
        return nil, err
    }
    coins := sdktypes.NewCoin(app.BondDenom, amount)
    msg := stakingtypes.NewMsgBeginRedelegate(from, srcValAddr, dstValAddr, coins)
    signedTx, err := ca.constructSignedTx(ctx, msg, apptypes.SetGasLimit(gasLim))
    if err != nil {
        return nil, err
    }
    return ca.SubmitTx(ctx, signedTx)
}

func (ca *CoreAccessor) Undelegate(
    ctx context.Context,
    delAddr ValAddress,
    amount Int,
    gasLim uint64,
) (*TxResponse, error) {
    if amount.IsNil() || amount.Int64() <= 0 {
        return nil, ErrInvalidAmount
    }

    from, err := ca.signer.GetSignerInfo().GetAddress()
    if err != nil {
        return nil, err
    }
    coins := sdktypes.NewCoin(app.BondDenom, amount)
    msg := stakingtypes.NewMsgUndelegate(from, delAddr, coins)
    signedTx, err := ca.constructSignedTx(ctx, msg, apptypes.SetGasLimit(gasLim))
    if err != nil {
        return nil, err
    }
    return ca.SubmitTx(ctx, signedTx)
}

func (ca *CoreAccessor) Delegate(
    ctx context.Context,
    delAddr ValAddress,
    amount Int,
    gasLim uint64,
) (*TxResponse, error) {
    if amount.IsNil() || amount.Int64() <= 0 {
        return nil, ErrInvalidAmount
    }

    from, err := ca.signer.GetSignerInfo().GetAddress()
    if err != nil {
        return nil, err
    }
    coins := sdktypes.NewCoin(app.BondDenom, amount)
    msg := stakingtypes.NewMsgDelegate(from, delAddr, coins)
    signedTx, err := ca.constructSignedTx(ctx, msg, apptypes.SetGasLimit(gasLim))
    if err != nil {
        return nil, err
    }
    return ca.SubmitTx(ctx, signedTx)
}

func (ca *CoreAccessor) QueryDelegation(
    ctx context.Context,
    valAddr ValAddress,
) (*stakingtypes.QueryDelegationResponse, error) {
    delAddr, err := ca.signer.GetSignerInfo().GetAddress()
    if err != nil {
        return nil, err
    }
    return ca.stakingCli.Delegation(ctx, &stakingtypes.QueryDelegationRequest{
        DelegatorAddr: delAddr.String(),
        ValidatorAddr: valAddr.String(),
    })
}

func (ca *CoreAccessor) QueryUnbonding(
    ctx context.Context,
    valAddr ValAddress,
) (*stakingtypes.QueryUnbondingDelegationResponse, error) {
    delAddr, err := ca.signer.GetSignerInfo().GetAddress()
    if err != nil {
        return nil, err
    }
    return ca.stakingCli.UnbondingDelegation(ctx, &stakingtypes.QueryUnbondingDelegationRequest{
        DelegatorAddr: delAddr.String(),
        ValidatorAddr: valAddr.String(),
    })
}
func (ca *CoreAccessor) QueryRedelegations(
    ctx context.Context,
    srcValAddr,
    dstValAddr ValAddress,
) (*stakingtypes.QueryRedelegationsResponse, error) {
    delAddr, err := ca.signer.GetSignerInfo().GetAddress()
    if err != nil {
        return nil, err
    }
    return ca.stakingCli.Redelegations(ctx, &stakingtypes.QueryRedelegationsRequest{
        DelegatorAddr:    delAddr.String(),
        SrcValidatorAddr: srcValAddr.String(),
        DstValidatorAddr: dstValAddr.String(),
    })
}

func (ca *CoreAccessor) IsStopped() bool {
    return ca.ctx.Err() != nil
}