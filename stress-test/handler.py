import time
import math

def is_prime(n):
    if n <= 1: return False
    for i in range(2, int(math.sqrt(n)) + 1):
        if n % i == 0: return False
    return True

def handle(event, context):
    """
    handle a request to the function
    Args:
        event (dict): request attributes
        context (object): runtime context
    """
    
    # 1. Get the request body
    # In python3-http, the body is in event.body (which is bytes)
    req_body = event.body
    
    limit = 20000 # Default load
    
    # 2. Parse input safely
    try:
        if req_body:
            # Decode bytes to string
            body_str = req_body.decode("utf-8").strip()
            if body_str.isdigit():
                limit = int(body_str)
    except Exception as e:
        pass # Ignore errors, use default

    # 3. Burn CPU
    start = time.time()
    primes_count = 0
    for i in range(limit):
        if is_prime(i):
            primes_count += 1
    
    duration = time.time() - start
    
    return {
        "statusCode": 200,
        "body": f"Checked {limit} numbers. Found {primes_count} primes. Took {duration:.4f}s"
    }