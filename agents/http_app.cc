/*
 * MIT License
 *
 * Copyright (c) 2019-2021 Ecole Polytechnique Federale Lausanne (EPFL)
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 *
 * THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
 * IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
 * FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
 * AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
 * LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
 * OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
 * SOFTWARE.
 */
#include <regex>
#include <cassert>
#include <string>
#include <string.h>
#include <strings.h>
#include <sstream>
#include <iostream>
#include <memory>

// Lancet-specific stuff
extern "C" {
#include <lancet/app_proto.h>
#include <lancet/tp_proto.h>
#include <lancet/error.h>
#include <lancet/misc.h>
#include <lancet/timestamping.h>
#include <lancet/rand_gen.h>
#include "picohttpparser.h"
}

struct HttpParams {
    std::string http_request;
    rand_gen* page_rand_gen = nullptr;

    HttpParams(std::string http_request, rand_gen* page_rand_gen)
        : http_request(std::move(http_request)), page_rand_gen(page_rand_gen) {}
};

class RequestCreator
{
private:
    const std::string mRequestString;
    std::unique_ptr<char[]> mStrBuf;
    rand_gen* page_rand_gen = nullptr;
public:
    explicit RequestCreator(const std::string &request_string, rand_gen* page_rand_gen = nullptr)
        : mRequestString(request_string), page_rand_gen(page_rand_gen) {
        mStrBuf = std::make_unique<char[]>(mRequestString.size() + 32);
        /* 32 bytes should be enough for the random number */
    }

    iovec renewRequest() {
        auto sz = mRequestString.size();
        if (page_rand_gen) {
            int random_number = generate(page_rand_gen);
            sz = snprintf(mStrBuf.get(), sz + 31, mRequestString.c_str(), random_number);
        } else {
            strncpy(mStrBuf.get(), mRequestString.data(), sz);
        }
        return { .iov_base = mStrBuf.get(), .iov_len = sz };
    }
};

int http_create_request(application_protocol *proto,
	request *req)
{
    using namespace std;
    static __thread RequestCreator* requestCreator = nullptr;

    auto const &http_params = *static_cast<const HttpParams*>(proto->arg); // if needed
    if (!requestCreator) {
        requestCreator = new RequestCreator(http_params.http_request, http_params.page_rand_gen);
    }
    req->iovs[0] = requestCreator->renewRequest();
    req->iov_cnt = 1;
    req->meta = nullptr; // callee must zero
    return 0;
}

const size_t max_headers = 32;
const char* content_length = "Content-Length";
const size_t content_length_length = 14;
byte_req_pair http_consume_response(application_protocol *proto, iovec *response) {
    using namespace std;
    const char *msg;
    size_t msg_len, num_headers = max_headers;
    phr_header headers[max_headers];
    int minor_version, status;

    auto ret = phr_parse_response(
            static_cast<const char*>(response->iov_base), response->iov_len,
            &minor_version, &status,
            &msg, &msg_len,
            headers, &num_headers, 0);
    if (ret < 0) {
        // -2 == how it indicates a partial request. anything else is unexpected
        if (ret != -2) {
            lancet_fprintf(stderr, "failed to parse HTTP response. Got return code %d\n", ret);
            assert(0);
        }
        return {0,0};
    }
    assert(ret > 0);

    bool found = false;
    size_t reported_content_length = 0;
    for (decltype(num_headers) i = 0; i < num_headers; ++i) {
        auto &hdr = headers[i];
        if (strncmp(hdr.name, content_length, min(content_length_length, hdr.name_len)) == 0) {
            reported_content_length = atoll(hdr.value);
            found = true;
            break;
        }
    }
    if (not found) {
        lancet_fprintf(stderr, "Unable to determine content of HTTP response from header\n");
        assert(0);
    }

    auto reported_total_len = ret + reported_content_length;
    auto leftover_bytes = response->iov_len - reported_total_len;
    if (reported_total_len > response->iov_len) {
        // in this case, we may need to wait for more if MAX_PAYLOAD has some left
        // if this is greater than MAX_PAYLOAD to buffer, then the calling method should error
        return {0,0};
    }

    assert(leftover_bytes >= 0);

    // otherwise, ret is number of bytes consumed
    return {
        .bytes = static_cast<decltype(byte_req_pair().bytes)>(reported_total_len),
        .reqs = 1
    };
}

// All interfacing stuff with the rest of the Lancet code goes below

static std::string extract_substring_in_braces(const std::string& str) {
    size_t startPos = str.find('{');
    size_t endPos = str.find('}', startPos);
    
    if (startPos != std::string::npos && endPos != std::string::npos && endPos > startPos) {
        return str.substr(startPos + 1, endPos - startPos - 1);
    }

    return "";
}

extern "C" int http_proto_init(char *proto, application_protocol *app_proto) {
    using namespace std;

    rand_gen* rand_gen = nullptr;

    assert(proto != nullptr);
    assert(app_proto != nullptr);

    regex http_resource(R"(^http:([\w\.]*)((?:/[-{}:\w\.]+)+)\s*$)");

    cmatch match;
    regex_match(proto, match, http_resource);
    if (not match.ready()) {
        lancet_fprintf(stderr, "Unable to parse http protocol\n");
        return -1;
    }
    assert(match.size() == 3); // so that the regex isn't malformed
    auto const&& request_host = match[1].str();
    auto asset_path = match[2].str();

    // check if asset_path contains a random generation spec inside {}
    // characters, and if so, extract that string from the asset_path.
    auto random_gen_spec_str = extract_substring_in_braces(asset_path);
    if (not random_gen_spec_str.empty()) {
        rand_gen = init_rand(&random_gen_spec_str[0]);
        if (rand_gen == nullptr) {
            lancet_fprintf(stderr, "Unable to initialize random generator for spec: %s\n", random_gen_spec_str.c_str());
            return -1;
        }

        // remove the random generation spec str from the asset_path so we're
        // left with `%d` which we can replace later with the random number.
        asset_path = asset_path.substr(0, asset_path.find('{')) + "%d" +
                     asset_path.substr(asset_path.find('}') + 1);
    }

    stringstream http_stream;
    http_stream << "GET " << asset_path << " HTTP/1.1\r\nHost: " << request_host << "\r\n\r\n";

    auto http_params = make_unique<HttpParams>(http_stream.str(), rand_gen);
    app_proto->arg = static_cast<decltype(app_proto->arg)>(http_params.release());
    app_proto->type = PROTO_HTTP;
    app_proto->create_request = http_create_request;
    app_proto->consume_response = http_consume_response;

    return 0;
}
