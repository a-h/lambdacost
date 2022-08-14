# lambdacost

Program to calculate the costs of Lambda functions, and to provide suggestions on savings that could be made by using smaller RAM allocations, and by migrating to ARM processors.

The program discovers Lambda functions and downloads their CloudWatch logs to analyse the GB seconds and invocation counts of each Lambda function.

It creates a JSON file of the report information at `{accountid}-{region}.json`, in case you want to adjust the program to modify the output report, and also writes the output report to stdout .

The first time you run `lambdacost` for a specific account ID and region, the program will output information about the logs that are being downloaded, before finally outputting the report.

> The program downloads the entire set of Lambda function logs from the time period in order to scan the data for durations. This costs real money, be careful where you run it. It's not my fault if you get a suprise bill.

## Output

```
Name                    Arch   Daily    Monthly    Invocations Avg          RAM           RAM            RAM           Monthly Savings           
                                                               Duration     Max           Assigned       Optimal       (arm64 + RAM)             
xxxxxxxxxx              arm64  $3.83298 $114.98946 99297       951.792235ms 64 (2.07%)    3096           1024          $76.56                    
xxxxxxxxxxxxxxxxxxxxxx  x86_64 $3.66138 $109.84139 57206       1.275565531s 184 (5.99%)   3072           1024          $80.30
xxxxxxxx                x86_64 $2.05089 $61.52674  153638      788.427641ms 81 (7.91%)    1024           1024          $12.12
xxxxxxxxx               x86_64 $1.39650 $41.89495  91192       301.776725ms 84 (2.73%)    3072           1024          $30.32
xxxxxxxxxx              x86_64 $0.33026 $9.90780   57200       110.976609ms 63 (2.05%)    3072           1024          $7.01
xxxxxxxx                x86_64 $0.28106 $8.43169   103366      1.208636824s 72 (56.25%)   128            128           $1.56
xxxxx                   x86_64 $0.08619 $2.58565   173107      17.27594ms   58 (5.66%)    1024           1024          $0.31
xxxxxxxxxxxxxxxxxxxxxxx x86_64 $0.05985 $1.79548   103541      180.951356ms 67 (52.34%)   128            128           $0.23
xxxxxxxxxxxxxxxxxxxx    x86_64 $0.04658 $1.39753   99332       4.901378ms   78 (2.54%)    3072           1024          $0.59
```

## Installation

### Binaries

See the Github releases for binary distributions for common platforms.

### From source

The program is written in Go. With Go installed an in the path, the binary can be built and installed directly.

```
go install github.com/a-h/lambdacost@latest
```

## Usage

```
lambdacost -region=eu-west-1
```

## Tasks

### build

```sh
go build
```

### release

Create production build with goreleaser.

```sh
if [ "${GITHUB_TOKEN}" == "" ]; then echo "No github token, run:"; echo "export GITHUB_TOKEN=`pass github.com/goreleaser_access_token`"; exit 1; fi
./push-tag.sh
goreleaser --rm-dist
```

